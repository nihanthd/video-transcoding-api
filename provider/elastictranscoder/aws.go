// Package elastictranscoder provides a implementation of the provider that
// uses AWS Elastic Transcoder for transcoding media files.
//
// It doesn't expose any public type. In order to use the provider, one must
// import this package and then grab the factory from the provider package:
//
//     import (
//         "github.com/nytm/video-transcoding-api/provider"
//         "github.com/nytm/video-transcoding-api/provider/elastictranscoder"
//     )
//
//     func UseProvider() {
//         factory, err := provider.GetProviderFactory(elastictranscoder.Name)
//         // handle err and use factory to get an instance of the provider.
//     }
package elastictranscoder

import (
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/credentials"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/elastictranscoder"
	"github.com/aws/aws-sdk-go/service/elastictranscoder/elastictranscoderiface"
	"github.com/nytm/video-transcoding-api/config"
	"github.com/nytm/video-transcoding-api/db"
	"github.com/nytm/video-transcoding-api/provider"
)

const (
	// Name is the name used for registering the Elastic Transcoder
	// provider in the registry of providers.
	Name = "elastictranscoder"

	defaultAWSRegion = "us-east-1"
)

var (
	errAWSInvalidConfig = errors.New("invalid Elastic Transcoder config. Please define the configuration entries in the config file or environment variables")
	s3Pattern           = regexp.MustCompile(`^s3://`)
)

func init() {
	provider.Register(Name, elasticTranscoderProvider)
}

type awsProvider struct {
	c      elastictranscoderiface.ElasticTranscoderAPI
	config *config.ElasticTranscoder
}

func (p *awsProvider) Transcode(job *db.Job, transcodeProfile provider.TranscodeProfile) (*provider.JobStatus, error) {
	var adaptiveStreamingOutputs []provider.TranscodeOutput
	source := p.normalizeSource(transcodeProfile.SourceMedia)
	params := elastictranscoder.CreateJobInput{
		PipelineId: aws.String(p.config.PipelineID),
		Input:      &elastictranscoder.JobInput{Key: aws.String(source)},
	}
	params.Outputs = make([]*elastictranscoder.CreateJobOutput, len(transcodeProfile.Outputs))
	for i, output := range transcodeProfile.Outputs {
		presetID, ok := output.Preset.ProviderMapping[Name]
		if !ok {
			return nil, provider.ErrPresetMapNotFound
		}
		presetQuery := &elastictranscoder.ReadPresetInput{
			Id: aws.String(presetID),
		}
		presetOutput, err := p.c.ReadPreset(presetQuery)
		if err != nil {
			return nil, err
		}
		if presetOutput.Preset == nil || presetOutput.Preset.Container == nil {
			return nil, fmt.Errorf("misconfigured preset: %s", presetID)
		}
		isAdaptiveStreamingPreset := false
		if *presetOutput.Preset.Container == "ts" {
			isAdaptiveStreamingPreset = true
			adaptiveStreamingOutputs = append(adaptiveStreamingOutputs, output)
		}
		params.Outputs[i] = &elastictranscoder.CreateJobOutput{
			PresetId: aws.String(presetID),
			Key:      p.outputKey(job, output.FileName, isAdaptiveStreamingPreset),
		}
		if isAdaptiveStreamingPreset {
			params.Outputs[i].SegmentDuration = aws.String(strconv.Itoa(int(transcodeProfile.StreamingParams.SegmentDuration)))
		}
	}

	if len(adaptiveStreamingOutputs) > 0 {
		playlistFileName := transcodeProfile.StreamingParams.PlaylistFileName
		playlistFileName = strings.TrimRight(playlistFileName, filepath.Ext(playlistFileName))
		jobPlaylist := elastictranscoder.CreateJobPlaylist{
			Format: aws.String("HLSv3"),
			Name:   aws.String(job.ID + "/" + playlistFileName),
		}

		jobPlaylist.OutputKeys = make([]*string, len(adaptiveStreamingOutputs))
		for i, output := range adaptiveStreamingOutputs {
			jobPlaylist.OutputKeys[i] = p.outputKey(job, output.FileName, true)
		}

		params.Playlists = []*elastictranscoder.CreateJobPlaylist{&jobPlaylist}
	}
	resp, err := p.c.CreateJob(&params)
	if err != nil {
		return nil, err
	}
	return &provider.JobStatus{
		ProviderName:  Name,
		ProviderJobID: aws.StringValue(resp.Job.Id),
		Status:        provider.StatusQueued,
	}, nil
}

func (p *awsProvider) normalizeSource(source string) string {
	if s3Pattern.MatchString(source) {
		source = strings.Replace(source, "s3://", "", 1)
		parts := strings.SplitN(source, "/", 2)
		return parts[len(parts)-1]
	}
	return source
}

func (p *awsProvider) outputKey(job *db.Job, fileName string, adaptive bool) *string {
	if adaptive {
		fileName = strings.TrimRight(fileName, filepath.Ext(fileName))
	}
	return aws.String(job.ID + "/" + fileName)
}

func (p *awsProvider) createVideoPreset(preset provider.Preset) *elastictranscoder.VideoParameters {
	videoPreset := elastictranscoder.VideoParameters{
		DisplayAspectRatio: aws.String("auto"),
		FrameRate:          aws.String("auto"),
		SizingPolicy:       aws.String("Fill"),
		PaddingPolicy:      aws.String("Pad"),
		Codec:              &preset.Video.Codec,
		KeyframesMaxDist:   &preset.Video.GopSize,
		CodecOptions: map[string]*string{
			"Profile":            aws.String(strings.ToLower(preset.Profile)),
			"Level":              &preset.ProfileLevel,
			"MaxReferenceFrames": aws.String("2"),
		},
	}
	if preset.Video.Width != "" {
		videoPreset.MaxWidth = &preset.Video.Width
	} else {
		videoPreset.MaxWidth = aws.String("auto")
	}
	if preset.Video.Height != "" {
		videoPreset.MaxHeight = &preset.Video.Height
	} else {
		videoPreset.MaxHeight = aws.String("auto")
	}
	normalizedVideoBitRate, _ := strconv.Atoi(preset.Video.Bitrate)
	videoBitrate := strconv.Itoa(normalizedVideoBitRate / 1000)
	videoPreset.BitRate = &videoBitrate
	if preset.Video.Codec == "h264" {
		videoPreset.Codec = aws.String("H.264")
	}
	if preset.Video.GopMode == "fixed" {
		videoPreset.FixedGOP = aws.String("true")
	}
	return &videoPreset
}

func (p *awsProvider) createThumbsPreset(preset provider.Preset) *elastictranscoder.Thumbnails {
	thumbsPreset := &elastictranscoder.Thumbnails{
		PaddingPolicy: aws.String("Pad"),
		Format:        aws.String("png"),
		Interval:      aws.String("1"),
		SizingPolicy:  aws.String("Fill"),
		MaxWidth:      aws.String("auto"),
		MaxHeight:     aws.String("auto"),
	}
	return thumbsPreset
}

func (p *awsProvider) createAudioPreset(preset provider.Preset) *elastictranscoder.AudioParameters {
	audioPreset := &elastictranscoder.AudioParameters{
		Codec:      &preset.Audio.Codec,
		Channels:   aws.String("auto"),
		SampleRate: aws.String("auto"),
	}

	normalizedAudioBitRate, _ := strconv.Atoi(preset.Audio.Bitrate)
	audioBitrate := strconv.Itoa(normalizedAudioBitRate / 1000)
	audioPreset.BitRate = &audioBitrate

	if preset.Audio.Codec == "aac" {
		audioPreset.Codec = aws.String("AAC")
	}

	return audioPreset
}

func (p *awsProvider) CreatePreset(preset provider.Preset) (string, error) {
	presetInput := elastictranscoder.CreatePresetInput{
		Name:        &preset.Name,
		Description: &preset.Description,
	}
	if preset.Container == "m3u8" {
		presetInput.Container = aws.String("ts")
	} else {
		presetInput.Container = &preset.Container
	}
	presetInput.Video = p.createVideoPreset(preset)
	presetInput.Audio = p.createAudioPreset(preset)
	presetInput.Thumbnails = p.createThumbsPreset(preset)
	presetOutput, err := p.c.CreatePreset(&presetInput)
	if err != nil {
		return "", err
	}
	return *presetOutput.Preset.Id, nil
}

func (p *awsProvider) GetPreset(presetID string) (interface{}, error) {
	readPresetInput := &elastictranscoder.ReadPresetInput{
		Id: aws.String(presetID),
	}
	readPresetOutput, err := p.c.ReadPreset(readPresetInput)
	if err != nil {
		return nil, err
	}
	return readPresetOutput, err
}

func (p *awsProvider) DeletePreset(presetID string) error {
	presetInput := elastictranscoder.DeletePresetInput{
		Id: &presetID,
	}
	_, err := p.c.DeletePreset(&presetInput)
	return err
}

func (p *awsProvider) JobStatus(job *db.Job) (*provider.JobStatus, error) {
	id := job.ProviderJobID
	resp, err := p.c.ReadJob(&elastictranscoder.ReadJobInput{Id: aws.String(id)})
	if err != nil {
		return nil, err
	}
	totalJobs := len(resp.Job.Outputs)
	completedJobs := float64(0)
	outputs := make(map[string]interface{}, totalJobs)
	for _, output := range resp.Job.Outputs {
		outputStatus := p.statusMap(aws.StringValue(output.Status))
		switch outputStatus {
		case provider.StatusFinished, provider.StatusCanceled, provider.StatusFailed:
			completedJobs++
		}
		outputs[aws.StringValue(output.Key)] = aws.StringValue(output.StatusDetail)
	}
	outputDestination, err := p.getOutputDestination(job, resp.Job)
	if err != nil {
		outputDestination = err.Error()
	}
	outputFiles, err := p.getOutputFiles(resp.Job)
	if err != nil {
		return nil, err
	}
	return &provider.JobStatus{
		ProviderJobID:  aws.StringValue(resp.Job.Id),
		Status:         p.statusMap(aws.StringValue(resp.Job.Status)),
		Progress:       completedJobs / float64(totalJobs) * 100,
		ProviderStatus: map[string]interface{}{"outputs": outputs},
		MediaInfo: provider.MediaInfo{
			Duration: time.Duration(aws.Int64Value(resp.Job.Input.DetectedProperties.DurationMillis)) * time.Millisecond,
			Height:   aws.Int64Value(resp.Job.Input.DetectedProperties.Height),
			Width:    aws.Int64Value(resp.Job.Input.DetectedProperties.Width),
		},
		Output: provider.JobOutput{
			Destination: outputDestination,
			Files:       outputFiles,
		},
	}, nil
}

func (p *awsProvider) getOutputDestination(job *db.Job, awsJob *elastictranscoder.Job) (string, error) {
	readPipelineOutput, err := p.c.ReadPipeline(&elastictranscoder.ReadPipelineInput{
		Id: awsJob.PipelineId,
	})
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("s3://%s/%s",
		aws.StringValue(readPipelineOutput.Pipeline.OutputBucket),
		job.ID,
	), nil
}

func (p *awsProvider) getOutputFiles(job *elastictranscoder.Job) ([]provider.OutputFile, error) {
	pipeline, err := p.c.ReadPipeline(&elastictranscoder.ReadPipelineInput{
		Id: job.PipelineId,
	})
	if err != nil {
		return nil, err
	}
	files := make([]provider.OutputFile, len(job.Outputs))
	for i, output := range job.Outputs {
		preset, err := p.c.ReadPreset(&elastictranscoder.ReadPresetInput{
			Id: output.PresetId,
		})
		if err != nil {
			return nil, err
		}
		filePath := fmt.Sprintf("s3://%s/%s%s",
			aws.StringValue(pipeline.Pipeline.OutputBucket),
			aws.StringValue(job.OutputKeyPrefix),
			aws.StringValue(output.Key),
		)
		files[i] = provider.OutputFile{
			Path:       filePath,
			Container:  aws.StringValue(preset.Preset.Container),
			VideoCodec: aws.StringValue(preset.Preset.Video.Codec),
			Width:      aws.Int64Value(output.Width),
			Height:     aws.Int64Value(output.Height),
		}
	}
	return files, nil
}

func (p *awsProvider) statusMap(awsStatus string) provider.Status {
	switch awsStatus {
	case "Submitted":
		return provider.StatusQueued
	case "Progressing":
		return provider.StatusStarted
	case "Complete":
		return provider.StatusFinished
	case "Canceled":
		return provider.StatusCanceled
	default:
		return provider.StatusFailed
	}
}

func (p *awsProvider) CancelJob(id string) error {
	_, err := p.c.CancelJob(&elastictranscoder.CancelJobInput{Id: aws.String(id)})
	return err
}

func (p *awsProvider) Healthcheck() error {
	_, err := p.c.ReadPipeline(&elastictranscoder.ReadPipelineInput{
		Id: aws.String(p.config.PipelineID),
	})
	return err
}

func (p *awsProvider) Capabilities() provider.Capabilities {
	return provider.Capabilities{
		InputFormats:  []string{"h264"},
		OutputFormats: []string{"mp4", "hls", "webm"},
		Destinations:  []string{"s3"},
	}
}

func elasticTranscoderProvider(cfg *config.Config) (provider.TranscodingProvider, error) {
	if cfg.ElasticTranscoder.AccessKeyID == "" || cfg.ElasticTranscoder.SecretAccessKey == "" || cfg.ElasticTranscoder.PipelineID == "" {
		return nil, errAWSInvalidConfig
	}
	creds := credentials.NewStaticCredentials(cfg.ElasticTranscoder.AccessKeyID, cfg.ElasticTranscoder.SecretAccessKey, "")
	region := cfg.ElasticTranscoder.Region
	if region == "" {
		region = defaultAWSRegion
	}
	awsSession := session.New(aws.NewConfig().WithCredentials(creds).WithRegion(region))
	return &awsProvider{
		c:      elastictranscoder.New(awsSession),
		config: cfg.ElasticTranscoder,
	}, nil
}
