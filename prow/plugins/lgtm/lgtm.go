/*
Copyright 2016 The Kubernetes Authors.

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

// Package lgtm implements the lgtm plugin
package lgtm

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/sirupsen/logrus"

	"k8s.io/test-infra/prow/config"
	"k8s.io/test-infra/prow/github"
	"k8s.io/test-infra/prow/labels"
	"k8s.io/test-infra/prow/pkg/layeredsets"
	"k8s.io/test-infra/prow/pluginhelp"
	"k8s.io/test-infra/prow/plugins"
	"k8s.io/test-infra/prow/repoowners"
)

const (
	// PluginName defines this plugin's registered name.
	PluginName = labels.LGTM
)

var (
	addLGTMLabelNotification   = "LGTM label has been added.  <details>Git tree hash: %s</details>"
	addLGTMLabelNotificationRe = regexp.MustCompile(fmt.Sprintf(addLGTMLabelNotification, "(.*)"))
	configInfoReviewActsAsLgtm = `Reviews of "approve" or "request changes" act as adding or removing LGTM.`
	configInfoStoreTreeHash    = `Squashing commits does not remove LGTM.`
	// LGTMLabel is the name of the lgtm label applied by the lgtm plugin
	LGTMLabel = labels.LGTM
	// LGTMRe is the regex that matches lgtm comments
	LGTMRe = regexp.MustCompile(`(?mi)^/lgtm(?: no-issue)?\s*$`)
	// LGTMCancelRe is the regex that matches lgtm cancel comments
	LGTMCancelRe        = regexp.MustCompile(`(?mi)^/(remove-lgtm|lgtm cancel)\s*$`)
	removeLGTMLabelNoti = "New changes are detected. LGTM label has been removed."
)

func configInfoStickyLgtmTeam(team string) string {
	return fmt.Sprintf(`Commits from "%s" do not remove LGTM.`, team)
}

type commentPruner interface {
	PruneComments(shouldPrune func(github.IssueComment) bool)
}

func init() {
	plugins.RegisterGenericCommentHandler(PluginName, handleGenericCommentEvent, helpProvider)
	plugins.RegisterPullRequestHandler(PluginName, func(pc plugins.Agent, pe github.PullRequestEvent) error {
		return handlePullRequestEvent(pc, pe)
	}, helpProvider)
	plugins.RegisterReviewEventHandler(PluginName, handlePullRequestReviewEvent, helpProvider)
}

func helpProvider(config *plugins.Configuration, enabledRepos []config.OrgRepo) (*pluginhelp.PluginHelp, error) {
	configInfo := map[string]string{}
	for _, repo := range enabledRepos {
		opts := config.LgtmFor(repo.Org, repo.Repo)
		var isConfigured bool
		var configInfoStrings []string
		configInfoStrings = append(configInfoStrings, "The plugin has the following configuration:<ul>")
		if opts.ReviewActsAsLgtm {
			configInfoStrings = append(configInfoStrings, "<li>"+configInfoReviewActsAsLgtm+"</li>")
			isConfigured = true
		}
		if opts.StoreTreeHash {
			configInfoStrings = append(configInfoStrings, "<li>"+configInfoStoreTreeHash+"</li>")
			isConfigured = true
		}
		if opts.StickyLgtmTeam != "" {
			configInfoStrings = append(configInfoStrings, "<li>"+configInfoStickyLgtmTeam(opts.StickyLgtmTeam)+"</li>")
			isConfigured = true
		}
		configInfoStrings = append(configInfoStrings, "</ul>")
		if isConfigured {
			configInfo[repo.String()] = strings.Join(configInfoStrings, "\n")
		}
	}
	yamlSnippet, err := plugins.CommentMap.GenYaml(&plugins.Configuration{
		Lgtm: []plugins.Lgtm{
			{
				Repos:            []string{"kubernetes/test-infra"},
				ReviewActsAsLgtm: true,
				StickyLgtmTeam:   "team1",
				StoreTreeHash:    true,
			},
		},
	})
	if err != nil {
		logrus.WithError(err).Warnf("cannot generate comments for %s plugin", PluginName)
	}
	pluginHelp := &pluginhelp.PluginHelp{
		Description: "The lgtm plugin manages the application and removal of the 'lgtm' (Looks Good To Me) label which is typically used to gate merging.",
		Config:      configInfo,
		Snippet:     yamlSnippet,
	}
	pluginHelp.AddCommand(pluginhelp.Command{
		Usage:       "/[remove-]lgtm [cancel] or GitHub Review action",
		Description: "Adds or removes the 'lgtm' label which is typically used to gate merging.",
		Featured:    true,
		WhoCanUse:   "Collaborators on the repository. '/lgtm cancel' can be used additionally by the PR author.",
		Examples:    []string{"/lgtm", "/lgtm cancel", "/remove-lgtm", "<a href=\"https://help.github.com/articles/about-pull-request-reviews/\">'Approve' or 'Request Changes'</a>"},
	})
	return pluginHelp, nil
}

type githubClient interface {
	IsCollaborator(owner, repo, login string) (bool, error)
	AddLabel(owner, repo string, number int, label string) error
	AssignIssue(owner, repo string, number int, assignees []string) error
	CreateComment(owner, repo string, number int, comment string) error
	RemoveLabel(owner, repo string, number int, label string) error
	GetIssueLabels(org, repo string, number int) ([]github.Label, error)
	GetPullRequest(org, repo string, number int) (*github.PullRequest, error)
	GetPullRequestChanges(org, repo string, number int) ([]github.PullRequestChange, error)
	ListIssueComments(org, repo string, number int) ([]github.IssueComment, error)
	DeleteComment(org, repo string, ID int) error
	BotUserChecker() (func(candidate string) bool, error)
	GetSingleCommit(org, repo, SHA string) (github.RepositoryCommit, error)
	IsMember(org, user string) (bool, error)
	ListTeams(org string) ([]github.Team, error)
	ListTeamMembers(org string, id int, role string) ([]github.TeamMember, error)
	RequestReview(org, repo string, number int, logins []string) error
}

// reviewCtx contains information about each review event
type reviewCtx struct {
	author, issueAuthor, body, htmlURL string
	repo                               github.Repo
	assignees                          []github.User
	number                             int
}

func handleGenericCommentEvent(pc plugins.Agent, e github.GenericCommentEvent) error {
	cp, err := pc.CommentPruner()
	if err != nil {
		return err
	}
	return handleGenericComment(pc.GitHubClient, pc.PluginConfig, pc.OwnersClient, pc.Logger, cp, e)
}

func handlePullRequestEvent(pc plugins.Agent, pre github.PullRequestEvent) error {
	return handlePullRequest(
		pc.Logger,
		pc.GitHubClient,
		pc.PluginConfig,
		&pre,
	)
}

func handlePullRequestReviewEvent(pc plugins.Agent, e github.ReviewEvent) error {
	// If ReviewActsAsLgtm is disabled, ignore review event.
	opts := pc.PluginConfig.LgtmFor(e.Repo.Owner.Login, e.Repo.Name)
	if !opts.ReviewActsAsLgtm {
		return nil
	}
	cp, err := pc.CommentPruner()
	if err != nil {
		return err
	}
	return handlePullRequestReview(pc.GitHubClient, pc.PluginConfig, pc.OwnersClient, pc.Logger, cp, e)
}

func handleGenericComment(gc githubClient, config *plugins.Configuration, ownersClient repoowners.Interface, log *logrus.Entry, cp commentPruner, e github.GenericCommentEvent) error {
	rc := reviewCtx{
		author:      e.User.Login,
		issueAuthor: e.IssueAuthor.Login,
		body:        e.Body,
		htmlURL:     e.HTMLURL,
		repo:        e.Repo,
		assignees:   e.Assignees,
		number:      e.Number,
	}

	// Only consider open PRs and new comments.
	if !e.IsPR || e.IssueState != "open" || e.Action != github.GenericCommentActionCreated {
		return nil
	}

	// If we create an "/lgtm" comment, add lgtm if necessary.
	// If we create a "/lgtm cancel" comment, remove lgtm if necessary.
	wantLGTM := false
	if LGTMRe.MatchString(rc.body) {
		wantLGTM = true
	} else if LGTMCancelRe.MatchString(rc.body) {
		wantLGTM = false
	} else {
		return nil
	}

	// use common handler to do the rest
	return handle(wantLGTM, config, ownersClient, rc, gc, log, cp)
}

func handlePullRequestReview(gc githubClient, config *plugins.Configuration, ownersClient repoowners.Interface, log *logrus.Entry, cp commentPruner, e github.ReviewEvent) error {
	rc := reviewCtx{
		author:      e.Review.User.Login,
		issueAuthor: e.PullRequest.User.Login,
		repo:        e.Repo,
		assignees:   e.PullRequest.Assignees,
		number:      e.PullRequest.Number,
		body:        e.Review.Body,
		htmlURL:     e.Review.HTMLURL,
	}

	// Only react to reviews that are being submitted (not editted or dismissed).
	if e.Action != github.ReviewActionSubmitted {
		return nil
	}

	// If the review event body contains an '/lgtm' or '/lgtm cancel' comment,
	// skip handling the review event
	if LGTMRe.MatchString(rc.body) || LGTMCancelRe.MatchString(rc.body) {
		return nil
	}

	// The review webhook returns state as lowercase, while the review API
	// returns state as uppercase. Uppercase the value here so it always
	// matches the constant.
	reviewState := github.ReviewState(strings.ToUpper(string(e.Review.State)))

	// If we review with Approve, add lgtm if necessary.
	// If we review with Request Changes, remove lgtm if necessary.
	wantLGTM := false
	if reviewState == github.ReviewStateApproved {
		wantLGTM = true
	} else if reviewState == github.ReviewStateChangesRequested {
		wantLGTM = false
	} else {
		return nil
	}

	// use common handler to do the rest
	return handle(wantLGTM, config, ownersClient, rc, gc, log, cp)
}

func handle(wantLGTM bool, config *plugins.Configuration, ownersClient repoowners.Interface, rc reviewCtx, gc githubClient, log *logrus.Entry, cp commentPruner) error {
	author := rc.author
	issueAuthor := rc.issueAuthor
	assignees := rc.assignees
	number := rc.number
	body := rc.body
	htmlURL := rc.htmlURL
	org := rc.repo.Owner.Login
	repoName := rc.repo.Name

	// Author cannot LGTM own PR, comment and abort
	isAuthor := author == issueAuthor
	if isAuthor && wantLGTM {
		resp := "you cannot LGTM your own PR."
		log.Infof("Commenting with \"%s\".", resp)
		return gc.CreateComment(rc.repo.Owner.Login, rc.repo.Name, rc.number, plugins.FormatResponseRaw(rc.body, rc.htmlURL, rc.author, resp))
	}

	// Determine if reviewer is already assigned
	isAssignee := false
	for _, assignee := range assignees {
		if assignee.Login == author {
			isAssignee = true
			break
		}
	}

	// check if skip collaborators is enabled for this org/repo
	skipCollaborators := skipCollaborators(config, org, repoName)

	// check if the commenter is a collaborator
	isCollaborator, err := gc.IsCollaborator(org, repoName, author)
	if err != nil {
		log.WithError(err).Error("Failed to check if author is a collaborator.")
		return err // abort if we can't determine if commenter is a collaborator
	}

	// if commenter isn't a collaborator, and we care about collaborators, abort
	if !isAuthor && !skipCollaborators && !isCollaborator {
		resp := "changing LGTM is restricted to collaborators"
		log.Infof("Reply to /lgtm request with comment: \"%s\"", resp)
		return gc.CreateComment(org, repoName, number, plugins.FormatResponseRaw(body, htmlURL, author, resp))
	}

	// either ensure that the commenter is a collaborator or an approver/reviewer
	if !isAuthor && !isAssignee && !skipCollaborators {
		// in this case we need to ensure the commenter is assignable to the PR
		// by assigning them
		log.Infof("Assigning %s/%s#%d to %s", org, repoName, number, author)
		if err := gc.AssignIssue(org, repoName, number, []string{author}); err != nil {
			log.WithError(err).Errorf("Failed to assign %s/%s#%d to %s", org, repoName, number, author)
		}
	} else if !isAuthor && skipCollaborators {
		// in this case we depend on OWNERS files instead to check if the author
		// is an approver or reviewer of the changed files
		log.Debugf("Skipping collaborator checks and loading OWNERS for %s/%s#%d", org, repoName, number)
		ro, err := loadRepoOwners(gc, ownersClient, org, repoName, number)
		if err != nil {
			return err
		}
		filenames, err := getChangedFiles(gc, org, repoName, number)
		if err != nil {
			return err
		}
		if !loadReviewers(ro, filenames).Has(github.NormLogin(author)) {
			resp := "adding LGTM is restricted to approvers and reviewers in OWNERS files."
			log.Infof("Reply to /lgtm request with comment: \"%s\"", resp)
			return gc.CreateComment(org, repoName, number, plugins.FormatResponseRaw(body, htmlURL, author, resp))
		}
	}

	// now we update the LGTM labels, having checked all cases where changing
	// LGTM was not allowed for the commenter

	// Only add the label if it doesn't have it, and vice versa.
	labels, err := gc.GetIssueLabels(org, repoName, number)
	if err != nil {
		log.WithError(err).Error("Failed to get issue labels.")
	}
	hasLGTM := github.HasLabel(LGTMLabel, labels)

	// remove the label if necessary, we're done after this
	opts := config.LgtmFor(rc.repo.Owner.Login, rc.repo.Name)
	if hasLGTM && !wantLGTM {
		log.Info("Removing LGTM label.")
		if err := removeLGTMAndRequestReview(gc, org, repoName, number, getLogins(assignees), opts.StoreTreeHash); err != nil {
			return err
		}
		if opts.StoreTreeHash {
			cp.PruneComments(func(comment github.IssueComment) bool {
				return addLGTMLabelNotificationRe.MatchString(comment.Body)
			})
		}
	} else if !hasLGTM && wantLGTM {
		log.Info("Adding LGTM label.")
		if err := gc.AddLabel(org, repoName, number, LGTMLabel); err != nil {
			return err
		}
		if !stickyLgtm(log, gc, config, opts, issueAuthor, org) {
			if opts.StoreTreeHash {
				pr, err := gc.GetPullRequest(org, repoName, number)
				if err != nil {
					log.WithError(err).Error("Failed to get pull request.")
				}
				commit, err := gc.GetSingleCommit(org, repoName, pr.Head.SHA)
				if err != nil {
					log.WithField("sha", pr.Head.SHA).WithError(err).Error("Failed to get commit.")
				}
				treeHash := commit.Commit.Tree.SHA
				log.WithField("tree", treeHash).Info("Adding comment to store tree-hash.")
				if err := gc.CreateComment(org, repoName, number, fmt.Sprintf(addLGTMLabelNotification, treeHash)); err != nil {
					log.WithError(err).Error("Failed to add comment.")
				}
			}
			// Delete the LGTM removed noti after the LGTM label is added.
			cp.PruneComments(func(comment github.IssueComment) bool {
				return strings.Contains(comment.Body, removeLGTMLabelNoti)
			})
		}
	}

	return nil
}

func stickyLgtm(log *logrus.Entry, gc githubClient, _ *plugins.Configuration, lgtm *plugins.Lgtm, author, org string) bool {
	if lgtm.StickyLgtmTeam == "" {
		return false
	}
	teams, err := gc.ListTeams(org)
	if err != nil {
		log.WithError(err).Errorf("Failed to list teams in org %s.", org)
		return false
	}
	for _, teamInOrg := range teams {
		if teamInOrg.Name == lgtm.StickyLgtmTeam {
			members, err := gc.ListTeamMembers(org, teamInOrg.ID, github.RoleAll)
			if err != nil {
				log.WithError(err).Errorf("Failed to list members in %s:%s.", org, teamInOrg.Name)
				return false
			}
			for _, member := range members {
				if member.Login == author {
					// The author is in the trusted team
					return true
				}
			}
			// The author is not in the trusted team
			return false
		}
	}
	log.Errorf("The configured trusted_team_for_sticky_lgtm (%q) was not found in the org %q.", lgtm.StickyLgtmTeam, org)
	return false
}

func handlePullRequest(log *logrus.Entry, gc githubClient, config *plugins.Configuration, pe *github.PullRequestEvent) error {
	if pe.PullRequest.Merged {
		return nil
	}

	if pe.Action != github.PullRequestActionSynchronize {
		return nil
	}

	org := pe.PullRequest.Base.Repo.Owner.Login
	repo := pe.PullRequest.Base.Repo.Name
	number := pe.PullRequest.Number

	opts := config.LgtmFor(org, repo)
	if stickyLgtm(log, gc, config, opts, pe.PullRequest.User.Login, org) {
		// If the author is trusted, skip tree hash verification and LGTM removal.
		return nil
	}

	// If we don't have the lgtm label, we don't need to check anything
	labels, err := gc.GetIssueLabels(org, repo, number)
	if err != nil {
		log.WithError(err).Error("Failed to get labels.")
	}
	if !github.HasLabel(LGTMLabel, labels) {
		return nil
	}

	if opts.StoreTreeHash {
		// Check if we have a tree-hash comment
		var lastLgtmTreeHash string
		botUserChecker, err := gc.BotUserChecker()
		if err != nil {
			return err
		}
		comments, err := gc.ListIssueComments(org, repo, number)
		if err != nil {
			log.WithError(err).Error("Failed to get issue comments.")
		}
		// older comments are still present
		// iterate backwards to find the last LGTM tree-hash
		for i := len(comments) - 1; i >= 0; i-- {
			comment := comments[i]
			m := addLGTMLabelNotificationRe.FindStringSubmatch(comment.Body)
			if botUserChecker(comment.User.Login) && m != nil && comment.UpdatedAt.Equal(comment.CreatedAt) {
				lastLgtmTreeHash = m[1]
				break
			}
		}
		if lastLgtmTreeHash != "" {
			// Get the current tree-hash
			commit, err := gc.GetSingleCommit(org, repo, pe.PullRequest.Head.SHA)
			if err != nil {
				log.WithField("sha", pe.PullRequest.Head.SHA).WithError(err).Error("Failed to get commit.")
			}
			treeHash := commit.Commit.Tree.SHA
			if treeHash == lastLgtmTreeHash {
				// Don't remove the label, PR code hasn't changed
				log.Infof("Keeping LGTM label as the tree-hash remained the same: %s", treeHash)
				return nil
			}
		}
	}

	if err := removeLGTMAndRequestReview(gc, org, repo, number, getLogins(pe.PullRequest.Assignees), opts.StoreTreeHash); err != nil {
		return fmt.Errorf("failed removing lgtm label: %w", err)
	}

	// Create a comment to inform participants that LGTM label is removed due to new
	// pull request changes.
	log.Infof("Commenting with an LGTM removed notification to %s/%s#%d with a message: %s", org, repo, number, removeLGTMLabelNoti)
	return gc.CreateComment(org, repo, number, removeLGTMLabelNoti)
}

func removeLGTMAndRequestReview(gc githubClient, org, repo string, number int, logins []string, storeTreeHash bool) error {
	if err := gc.RemoveLabel(org, repo, number, LGTMLabel); err != nil {
		return fmt.Errorf("failed removing lgtm label: %w", err)
	}

	// Re-request review because LGTM has been removed only if storeTreeHash enabled.
	// TODO(mpherman): Surface User errors to PR
	if storeTreeHash {
		if err := gc.RequestReview(org, repo, number, logins); err != nil {
			return fmt.Errorf("failed to re-request review")
		}
	}
	return nil
}

func getLogins(usrs []github.User) []string {
	res := []string{}
	for _, usr := range usrs {
		res = append(res, usr.Login)
	}
	return res
}

func skipCollaborators(config *plugins.Configuration, org, repo string) bool {
	full := fmt.Sprintf("%s/%s", org, repo)
	for _, elem := range config.Owners.SkipCollaborators {
		if elem == org || elem == full {
			return true
		}
	}
	return false
}

func loadRepoOwners(gc githubClient, ownersClient repoowners.Interface, org, repo string, number int) (repoowners.RepoOwner, error) {
	pr, err := gc.GetPullRequest(org, repo, number)
	if err != nil {
		return nil, err
	}
	return ownersClient.LoadRepoOwners(org, repo, pr.Base.Ref)
}

// getChangedFiles returns all the changed files for the provided pull request.
func getChangedFiles(gc githubClient, org, repo string, number int) ([]string, error) {
	changes, err := gc.GetPullRequestChanges(org, repo, number)
	if err != nil {
		return nil, fmt.Errorf("cannot get PR changes for %s/%s#%d", org, repo, number)
	}
	var filenames []string
	for _, change := range changes {
		filenames = append(filenames, change.Filename)
	}
	return filenames, nil
}

// loadReviewers returns all reviewers and approvers from all OWNERS files that
// cover the provided filenames.
func loadReviewers(ro repoowners.RepoOwner, filenames []string) layeredsets.String {
	reviewers := layeredsets.String{}
	for _, filename := range filenames {
		reviewers = reviewers.Union(ro.Approvers(filename)).Union(ro.Reviewers(filename))
	}
	return reviewers
}
