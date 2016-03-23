package service

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/gorilla/mux"
	"github.com/nytm/video-transcoding-api/db"
	"github.com/nytm/video-transcoding-api/provider"
	"golang.org/x/net/context"
)

const maxJobTimeout = 8 * time.Hour

// swagger:route POST /jobs jobs newJob
//
// Creates a new transcoding job.
//
//     Responses:
//       200: job
//       400: invalidJob
//       500: genericError
func (s *TranscodingService) newTranscodeJob(r *http.Request) gizmoResponse {
	defer r.Body.Close()
	var input newTranscodeJobInput
	providerFactory, err := input.ProviderFactory(r.Body)
	if err != nil {
		return newInvalidJobResponse(err)
	}
	providerObj, err := providerFactory(s.config)
	if err != nil {
		formattedErr := fmt.Errorf("Error initializing provider %s for new job: %v %s", input.Payload.Provider, providerObj, err)
		if _, ok := err.(provider.InvalidConfigError); ok {
			return newInvalidJobResponse(formattedErr)
		}
		return newErrorResponse(formattedErr)
	}
	presets := make([]db.Preset, len(input.Payload.Presets))
	for i, presetID := range input.Payload.Presets {
		preset, err := s.db.GetPreset(presetID)
		if err != nil {
			if err == db.ErrPresetNotFound {
				return newInvalidJobResponse(err)
			}
			return newErrorResponse(err)
		}
		presets[i] = *preset
	}
	transcodeProfile := provider.TranscodeProfile{
		SourceMedia:     input.Payload.Source,
		Presets:         presets,
		StreamingParams: input.Payload.StreamingParams,
	}
	jobStatus, err := providerObj.Transcode(transcodeProfile)
	if err == provider.ErrPresetNotFound {
		return newInvalidJobResponse(err)
	}
	if err != nil {
		providerError := fmt.Errorf("Error with provider %q: %s", input.Payload.Provider, err)
		return newErrorResponse(providerError)
	}
	jobStatus.ProviderName = input.Payload.Provider
	job := db.Job{
		ProviderName:           jobStatus.ProviderName,
		ProviderJobID:          jobStatus.ProviderJobID,
		StatusCallbackURL:      input.Payload.StatusCallbackURL,
		StatusCallbackInterval: input.Payload.StatusCallbackInterval,
		CompletionCallbackURL:  input.Payload.CompletionCallbackURL,
	}
	if transcodeProfile.StreamingParams.Protocol != "" {
		job.StreamingParams = db.StreamingParams{
			SegmentDuration: transcodeProfile.StreamingParams.SegmentDuration,
			Protocol:        transcodeProfile.StreamingParams.Protocol,
		}
	}
	err = s.db.CreateJob(&job)
	if err != nil {
		return newErrorResponse(err)
	}
	if job.StatusCallbackURL != "" || job.CompletionCallbackURL != "" {
		ctx, cancel := context.WithTimeout(context.Background(), maxJobTimeout)
		defer cancel()
		go s.statusCallback(ctx, job)
	}
	return newJobResponse(job.ID)
}

// swagger:route GET /jobs/{jobId} jobs getJob
//
// Finds a trancode job using its ID.
// It also queries the provider to get the status of the job.
//
//     Responses:
//       200: jobStatus
//       404: jobNotFound
//       410: jobNotFoundInTheProvider
//       500: genericError
func (s *TranscodingService) getTranscodeJob(r *http.Request) gizmoResponse {
	var params getTranscodeJobInput
	params.loadParams(mux.Vars(r))
	return s.getJobStatusResponse(s.getTranscodeJobByID(params.JobID))
}

func (s *TranscodingService) getJobStatusResponse(job *db.Job, jobStatus *provider.JobStatus, providerObj provider.TranscodingProvider, err error) gizmoResponse {
	if err != nil {
		if err == db.ErrJobNotFound {
			return newJobNotFoundResponse(err)
		}
		if providerObj != nil {
			providerError := fmt.Errorf("Error with provider %q when trying to retrieve job id %q: %s", job.ProviderName, job.ID, err)
			if _, ok := err.(provider.JobNotFoundError); ok {
				return newJobNotFoundProviderResponse(providerError)
			}
			return newErrorResponse(providerError)
		}
		return newErrorResponse(err)
	}
	return newJobStatusResponse(jobStatus)
}

func (s *TranscodingService) getTranscodeJobByID(jobID string) (*db.Job, *provider.JobStatus, provider.TranscodingProvider, error) {
	job, err := s.db.GetJob(jobID)
	if err != nil {
		if err == db.ErrJobNotFound {
			return nil, nil, nil, err
		}
		return nil, nil, nil, fmt.Errorf("error retrieving job with id %q: %s", jobID, err)
	}
	providerFactory, err := provider.GetProviderFactory(job.ProviderName)
	if err != nil {
		return job, nil, nil, fmt.Errorf("unknown provider %q for job id %q", job.ProviderName, jobID)
	}
	providerObj, err := providerFactory(s.config)
	if err != nil {
		return job, nil, nil, fmt.Errorf("error initializing provider %q on job id %q: %s %s", job.ProviderName, jobID, providerObj, err)
	}
	jobStatus, err := providerObj.JobStatus(job.ProviderJobID)
	if err != nil {
		return job, nil, providerObj, err
	}
	jobStatus.ProviderName = job.ProviderName
	return job, jobStatus, providerObj, nil
}

func (s *TranscodingService) statusCallback(ctx context.Context, job db.Job) error {
	deadline, _ := ctx.Deadline()
	for now := time.Now(); now.Before(deadline); now = time.Now() {
		job, jobStatus, providerObj, err := s.getTranscodeJobByID(job.ID)
		gizmoResponseObj := s.getJobStatusResponse(job, jobStatus, providerObj, err)
		if job.StatusCallbackURL != "" {
			err := s.postStatusToCallback(gizmoResponseObj, job.StatusCallbackURL)
			if err != nil {
				continue
			}
		}
		if jobStatus.Status != provider.StatusQueued &&
			jobStatus.Status != provider.StatusStarted {
			if job.CompletionCallbackURL != "" {
				err := s.postStatusToCallback(gizmoResponseObj, job.CompletionCallbackURL)
				if err != nil {
					continue
				}
			}
			break
		}
		time.Sleep(time.Duration(job.StatusCallbackInterval) * time.Second)
	}
	return nil
}

func (s *TranscodingService) postStatusToCallback(payloadStruct gizmoResponse, callbackURL string) error {
	jsonPayload, err := json.Marshal(payloadStruct)
	if err != nil {
		fmt.Printf("Error generating response for status callback: %v", err)
		return err
	}
	req, err := http.NewRequest("POST", callbackURL, bytes.NewBuffer(jsonPayload))
	req.Header.Set("Content-Type", "application/json")
	timeout := time.Duration(5 * time.Second)
	client := &http.Client{
		Timeout: timeout,
	}
	resp, err := client.Do(req)
	if err != nil {
		fmt.Printf("Error calling status callback URL %s : %v", callbackURL, err)
		return err
	}
	resp.Body.Close()
	return nil
}
