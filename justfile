default:
	@just --list

# Build the OKAY RUN CLI binary
build:
	go build -o bin/okay .

# Stage, commit, push, PR, and merge changes in one go (handles repos with PR-only rulesets)
pr-land message branch_name="":
	#!/usr/bin/env bash
	set -euo pipefail
	command -v gh >/dev/null 2>&1 || { echo "Error: gh CLI is required."; exit 1; }
	BRANCH="{{branch_name}}"
	if [[ -z "$BRANCH" ]]; then
	  SLUG=$(echo "{{message}}" | tr '[:upper:]' '[:lower:]' | sed -E 's/[^a-z0-9]+/-/g' | sed -E 's/^-|-$//g' | cut -c1-30)
	  BRANCH="auto-$SLUG"
	fi
	CURRENT_BRANCH=$(git branch --show-current)
	if [[ "$CURRENT_BRANCH" == "main" ]]; then
	  echo "Creating and switching to branch: $BRANCH"
	  git checkout -b "$BRANCH"
	else
	  echo "Using current branch: $CURRENT_BRANCH"
	  BRANCH="$CURRENT_BRANCH"
	fi
	echo "Staging and committing..."
	git add .
	git commit -m "{{message}}" || echo "No changes to commit."
	echo "Pushing branch $BRANCH..."
	git push -u origin "$BRANCH"
	echo "Creating Pull Request..."
	PR_URL=$(gh pr create --title "{{message}}" --body "Automated land via 'just pr-land'.")
	echo "PR Created: $PR_URL"
	echo "Merging Pull Request..."
	gh pr merge --merge --delete-branch
	echo "Syncing main..."
	git checkout main
	git pull origin main
	echo "Successfully landed changes!"
