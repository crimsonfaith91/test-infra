/*
Copyright 2017 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package mungers

import (
	"fmt"
	"github.com/golang/glog"
	githubapi "github.com/google/go-github/github"
	"github.com/spf13/cobra"
	"k8s.io/test-infra/mungegithub/features"
	"k8s.io/test-infra/mungegithub/github"
	"k8s.io/test-infra/mungegithub/mungers/matchers"
	"time"
)

const (
	msgHeader   = "Sorry it is taking a long time for @%s to review your PR. This person may be on vacation or otherwise occupied. "
	msgExpedite = "To expedite a review, please "
)

type InactiveReviewHandler struct {
	features *features.Features
}

func init() {
	h := &InactiveReviewHandler{}
	RegisterMungerOrDie(h)
}

// Name is the name usable in --pr-mungers
func (i *InactiveReviewHandler) Name() string { return "inactive-review-handler" }

// RequiredFeatures is a slice of 'features' that must be provided
func (i *InactiveReviewHandler) RequiredFeatures() []string {
	return []string{features.RepoFeatureName, features.AliasesFeature}
}

// Initialize will initialize the munger
func (i *InactiveReviewHandler) Initialize(config *github.Config, features *features.Features) error {
	i.features = features
	return nil
}

// EachLoop is called at the start of every munge loop
func (i *InactiveReviewHandler) EachLoop() error { return nil }

// AddFlags will add any request flags to the cobra `cmd`
func (i *InactiveReviewHandler) AddFlags(cmd *cobra.Command, config *github.Config) {}

// Determine whether the PR is active based on number of seconds
// A PR is inactive if it is not being modified for more than one week
// The munger will only run at most 5 times
func (i *InactiveReviewHandler) isPRActive(prCreatedAt *time.Time, comments []*githubapi.IssueComment, reviewComments []*githubapi.PullRequestComment) bool {
	pinger := matchers.NewPinger("INACTIVE-REVIEWER").SetTimePeriod(7 * 24 * time.Hour).SetMaxCount(5)
	lastDate := matchers.Items{}.
		AddComments(comments...).
		AddReviewComments(reviewComments...).
		Filter(matchers.HumanActor()).
		LastDate(prCreatedAt)

	return !pinger.IsMaxReached(comments, lastDate)
}

// Suggest a new reviewer who is NOT any of the existing reviewers
// (1) get all current assignees for the PR
// (2) get potential owners of the PR using Blunderbuss algorithm (calling getPotentialOwners() function)
// (3) filter out current assignees from the potential owners
// (4) if there is no any new reviewer available, the bot will encourage the PR author to ping all existing assignees
// (5) otherwise, select a new reviewer using Blunderbuss algorithm (calling selectMultipleOwners() function with number of assignees parameter of one)
// Note: the munger will suggest a new reviewer when the PR currently does not have any reviewer
//		 the munger will only run at most 5 times
func (i *InactiveReviewHandler) suggestNewReviewer(issue *githubapi.Issue, potentialOwners weightMap, weightSum int64) string {
	var newReviewer string

	if len(potentialOwners) > 0 && issue.Assignees != nil && len(issue.Assignees) > 0 {
		for _, oldReviewer := range issue.Assignees {
			login := *oldReviewer.Login

			for potentialOwner := range potentialOwners {
				if login == potentialOwner {
					weightSum -= potentialOwners[login]
					delete(potentialOwners, login)
					break
				}
			}
		}
	}

	if len(potentialOwners) == 0 {
		return newReviewer
	}

	newReviewer = selectMultipleOwners(potentialOwners, weightSum, 1)[0]

	return newReviewer
}

// Munge is the workhorse encouraging PR author to assign a new reviewer
// after getting no response from current reviewer for one week
// The algorithm:
// (1) find last modification time of the reviewer
// (2) if the date is one week or longer before today's date, create a comment
//     encouraging the author to assign a new reviewer and unassign the old reviewer
// (3) suggest the new reviewer using Blunderbuss algorithm, making sure the old reviewer is not suggested
func (i *InactiveReviewHandler) Munge(obj *github.MungeObject) {
	issue := obj.Issue

	//do not suggest new reviewer if it is not a PR or the PR has no author information
	if !obj.IsPR() || issue.User == nil || issue.User.Login == nil {
		return
	}

	pr, ok := obj.GetPR()
	if !ok {
		return
	}

	comments, ok := obj.ListComments()
	if !ok {
		return
	}

	reviewComments, ok := obj.ListReviewComments()
	if !ok {
		return
	}

	if i.isPRActive(pr.CreatedAt, comments, reviewComments) {
		return
	}

	files, ok := obj.ListFiles()
	if !ok || len(files) == 0 {
		glog.Errorf("failed to detect any changed file when assigning a new reviewer for inactive PR #%v", *obj.Issue.Number)
		return
	}

	potentialOwners, weightSum := getPotentialOwnersHelper(issue.User.Login, i.features, files, issue.Number)

	newReviewer := i.suggestNewReviewer(issue, potentialOwners, weightSum)

	var msg string

	// Current implementation suggests removing first assignee of issue.Assignees
	if len(issue.Assignees) == 0 {
		msg = fmt.Sprintf(msgExpedite+"`/assign @`%s.", newReviewer)
	} else if len(newReviewer) == 0 {
		msg = fmt.Sprintf(msgHeader+msgExpedite+"ping him / her.", *issue.Assignees[0].Login)
	} else {
		msg = fmt.Sprintf(msgHeader+msgExpedite+"`/assign @`%s and consider `/unassign @`%s.", *issue.Assignees[0].Login, newReviewer, *issue.Assignees[0].Login)
	}

	if err := obj.WriteComment(msg); err != nil {
		glog.Errorf("failed to leave comment encouraging %s to assign a new reviewer for inactive PR #%v", *issue.User.Login, *issue.Number)
	}
}
