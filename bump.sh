#!/bin/bash

PROJECT=$(git config --local remote.origin.url|sed -n 's#.*/\([^.]*\)\.git#\1#p')
GH_LOG_TEMPLATE="([%h](https://github.com/nytimes/$PROJECT/commit/%h)) %s %n"
EMAIL_LOG_TEMPLATE="[<a href=https://github.com/nytimes/$PROJECT/commit/%h>%h</a>] %s - by %an, %ci.<br>"
RECIPIENT=mediafactory@nytimes.com

increment_version() {
  arr=(${1//[.,-]/ })
  if [ "$2" == "major" ]; then
    ((arr[0]++))
    arr[1]=0
    arr[2]=0
  elif [ "$2" == "minor" ]; then
    ((arr[1]++))
    arr[2]=0
  elif [ "$2" == "bugfix" ]; then
    ((arr[2]++))
  fi

  if [ "$2" == "major" ]; then
    echo "${arr[0]}.${arr[1]}.${arr[2]}"
  else
    echo "${arr[0]}.${arr[1]}.${arr[2]}-rc"
  fi
}

update_changelog() {
  OLD_CHANGELOG="$(cat CHANGELOG.md)"

  echo "## Version $2 (Release date: $(date +%F))" > CHANGELOG.md
  git log $1..master --pretty="$GH_LOG_TEMPLATE" | grep -v Merge | grep -v bump >> CHANGELOG.md
  printf "\n\n\n${OLD_CHANGELOG}" >> CHANGELOG.md
}

bump_version() {
  git add CHANGELOG.md
  git commit -m "bump to $1"
  git tag $1
  git push origin master --tags
}

send_mail() {
  git log $1..$2 --pretty="$EMAIL_LOG_TEMPLATE" | grep -v Merge | grep -v bump >> .tmp_mail

  TITLE="[$PROJECT] New version released: $2"

  HEADER="<img src=http://flv.io/mf.png><h2>Changelog</h2>"
  BODY=$(cat .tmp_mail)
  FOOTER="You can also see the full changelog on <a href=https://github.com/NYTimes/$PROJECT/blob/master/CHANGELOG.md>GitHub</a>.<br><br>Media Factory Team."
  MESSAGE="${HEADER}<br>${BODY}<br>${FOOTER}"

  SUBJECT="$TITLE\nFrom: Media Factory <mediafactory@nytimes.com>\nContent-Type: text/html\n"

  rm -rf .tmp_mail
  echo -e $MESSAGE | mail -s "$(echo -e "$SUBJECT")" $RECIPIENT
}

if [ "$1" != "" ]; then
  read -p "You're about to bump a new version and push to master. Is that what you intended? [y|n] " -n 1 -r < /dev/tty
  if echo $REPLY | grep -E '^[Yy]$' > /dev/null; then
    last_version=$(git describe --tags $(git rev-list --tags --max-count=1))
    new_version=$(increment_version $last_version $1)

    git checkout master
    git pull --rebase
    update_changelog $last_version $new_version
    bump_version $new_version
    send_mail $last_version $new_version
  else
    echo " Bump aborted."
  fi
else
  echo "Usage: ./bump.sh [major|minor|bugfix]"
fi

