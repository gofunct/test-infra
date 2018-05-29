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

// This is a label_sync tool, details in README.md
package main

import (
	"bytes"
	"errors"
	"flag"
	"fmt"
	"io/ioutil"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"text/template"
	"time"

	"github.com/ghodss/yaml"
	"github.com/sirupsen/logrus"

	"k8s.io/test-infra/prow/flagutil"
	"k8s.io/test-infra/prow/github"
)

const maxConcurrentWorkers = 20

// A label in a repository.

type LabelTarget string

const (
	PRTarget    LabelTarget = "prs"
	IssueTarget             = "issues"
	BothTarget              = "both"
)

var LabelTargets []LabelTarget = []LabelTarget{PRTarget, IssueTarget, BothTarget}

type Label struct {
	Name        string      `json:"name"`                  // Current name of the label
	Color       string      `json:"color"`                 // rrggbb or color
	Description string      `json:"description"`           // What does this label mean, who can apply it
	Target      LabelTarget `json:"target"`                // What can this label be applied to: issues, prs, or both
	ProwPlugin  string      `json:"prowPlugin"`            // Which prow plugin is used to add/remove this label
	AddedBy     string      `json:"addedBy"`               // What human or plugin or munger or bot adds this label
	Previously  []Label     `json:"previously,omitempty"`  // Previous names for this label
	DeleteAfter *time.Time  `json:"deleteAfter,omitempty"` // Retired labels deleted on this date
	parent      *Label      // Current name for previous labels (used internally)
}

// Configuration is a list of Required Labels to sync in all kubernetes repos
type Configuration struct {
	Labels []Label `json:"labels"`
}

type RepoList []github.Repo
type RepoLabels map[string][]github.Label

// Update a label in a repo
type Update struct {
	repo    string
	Why     string
	Wanted  *Label `json:"wanted,omitempty"`
	Current *Label `json:"current,omitempty"`
}

// RepoUpdates Repositories to update: map repo name --> list of Updates
type RepoUpdates map[string][]Update

var (
	debug        = flag.Bool("debug", false, "Turn on debug to be more verbose")
	confirm      = flag.Bool("confirm", false, "Make mutating API calls to GitHub.")
	endpoint     = flagutil.NewStrings("https://api.github.com")
	labelsPath   = flag.String("config", "", "Path to labels.yaml")
	onlyRepos    = flag.String("only", "", "Only look at the following comma separated org/repos")
	orgs         = flag.String("orgs", "", "Comma separated list of orgs to sync")
	skipRepos    = flag.String("skip", "", "Comma separated list of org/repos to skip syncing")
	token        = flag.String("token", "", "Path to github oauth secret")
	action       = flag.String("action", "sync", "One of: sync, docs")
	docsTemplate = flag.String("docs-template", "", "Path to template file for label docs")
	docsOutput   = flag.String("docs-output", "", "Path to output file for docs")
)

func init() {
	flag.Var(&endpoint, "endpoint", "GitHub's API endpoint")
}

func pathExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// Writes the golang text template at templatePath to outputPath using the given data
func writeTemplate(templatePath string, outputPath string, data interface{}) error {
	// set up template
	funcMap := template.FuncMap{
		"anchor": func(input string) string {
			return strings.Replace(input, ":", " ", -1)
		},
	}
	t, err := template.New(filepath.Base(templatePath)).Funcs(funcMap).ParseFiles(templatePath)
	if err != nil {
		return err
	}

	// ensure output path exists
	if !pathExists(outputPath) {
		_, err = os.Create(outputPath)
		if err != nil {
			return err
		}
	}

	// open file at output path and truncate
	f, err := os.OpenFile(outputPath, os.O_RDWR, 0644)
	if err != nil {
		return err
	}
	defer f.Close()
	f.Truncate(0)

	// render template to output path
	err = t.Execute(f, data)
	if err != nil {
		return err
	}

	return nil
}

// Ensures that no two label names (including previous names) have the same lowercase value.
func validate(labels []Label, parent string, seen map[string]string) error {
	for _, l := range labels {
		name := strings.ToLower(l.Name)
		path := parent + "." + name
		if other, present := seen[name]; present {
			return fmt.Errorf("duplicate label %s at %s and %s", name, path, other)
		}
		seen[name] = path
		if err := validate(l.Previously, path, seen); err != nil {
			return err
		}
	}
	return nil
}

// Ensures the config does not duplicate label names
func (c Configuration) validate() error {
	seen := make(map[string]string)
	if err := validate(c.Labels, "", seen); err != nil {
		return fmt.Errorf("invalid config: %v", err)
	}
	return nil
}

// Return labels that have a given target
func (c Configuration) LabelsByTarget(target LabelTarget) (labels []Label) {
	for _, label := range c.Labels {
		if target == label.Target {
			labels = append(labels, label)
		}
	}
	return
}

// Load yaml config at path
func LoadConfig(path string) (*Configuration, error) {
	if path == "" {
		return nil, errors.New("empty path")
	}
	var c Configuration
	data, err := ioutil.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if err = yaml.Unmarshal(data, &c); err != nil {
		return nil, err
	}
	if err = c.validate(); err != nil { // Ensure no dups
		return nil, err
	}
	return &c, nil
}

// GetOrg returns organization from "org" or "user:name"
// Org can be organization name like "kubernetes"
// But we can also request all user's public repos via user:github_user_name
func GetOrg(org string) (string, bool) {
	data := strings.Split(org, ":")
	if len(data) == 2 && data[0] == "user" {
		return data[1], true
	}
	return org, false
}

// Get reads repository list for given org
// Use provided githubClient (real, dry, fake)
// Uses GitHub: /orgs/:org/repos
func LoadRepos(org string, gc client, filt filter) (RepoList, error) {
	org, isUser := GetOrg(org)
	repos, err := gc.GetRepos(org, isUser)
	if err != nil {
		return nil, err
	}
	var rl RepoList
	for _, r := range repos {
		if !filt(org, r.Name) {
			continue
		}
		rl = append(rl, r)
	}
	return rl, nil
}

// Get reads repository's labels list
// Use provided githubClient (real, dry, fake)
// Uses GitHub: /repos/:org/:repo/labels
func LoadLabels(gc client, org string, repos RepoList) (*RepoLabels, error) {
	repoChan := make(chan github.Repo, len(repos))
	for _, repo := range repos {
		repoChan <- repo
	}
	close(repoChan)

	wg := sync.WaitGroup{}
	wg.Add(maxConcurrentWorkers)
	labels := make(chan RepoLabels, len(repos))
	errChan := make(chan error, len(repos))
	for i := 0; i < maxConcurrentWorkers; i++ {
		go func(repositories <-chan github.Repo) {
			defer wg.Done()
			for repository := range repositories {
				logrus.WithField("org", org).WithField("repo", repository.Name).Info("Listing labels for repo")
				repoLabels, err := gc.GetRepoLabels(org, repository.Name)
				if err != nil {
					logrus.WithField("org", org).WithField("repo", repository.Name).Error("Failed listing labels for repo")
					errChan <- err
				}
				labels <- RepoLabels{repository.Name: repoLabels}
			}
		}(repoChan)
	}

	wg.Wait()
	close(labels)
	close(errChan)

	rl := RepoLabels{}
	for data := range labels {
		for repo, repoLabels := range data {
			rl[repo] = repoLabels
		}
	}

	var overallErr error
	if len(errChan) > 0 {
		var listErrs []error
		for listErr := range errChan {
			listErrs = append(listErrs, listErr)
		}
		overallErr = fmt.Errorf("failed to list labels: %v", listErrs)
	}

	return &rl, overallErr
}

// Delete the label
func kill(repo string, label Label) Update {
	logrus.WithField("repo", repo).WithField("label", label.Name).Info("kill")
	return Update{Why: "dead", Current: &label, repo: repo}
}

// Create the label
func create(repo string, label Label) Update {
	logrus.WithField("repo", repo).WithField("label", label.Name).Info("create")
	return Update{Why: "missing", Wanted: &label, repo: repo}
}

// Rename the label (will also update color)
func rename(repo string, previous, wanted Label) Update {
	logrus.WithField("repo", repo).WithField("from", previous.Name).WithField("to", wanted.Name).Info("rename")
	return Update{Why: "rename", Current: &previous, Wanted: &wanted, repo: repo}
}

// Update the label color/description
func change(repo string, label Label) Update {
	logrus.WithField("repo", repo).WithField("label", label.Name).WithField("color", label.Color).Info("change")
	return Update{Why: "change", Current: &label, Wanted: &label, repo: repo}
}

// Migrate labels to another label
func move(repo string, previous, wanted Label) Update {
	logrus.WithField("repo", repo).WithField("from", previous.Name).WithField("to", wanted.Name).Info("migrate")
	return Update{Why: "migrate", Wanted: &wanted, Current: &previous, repo: repo}
}

func ClassifyLabels(labels []Label, required, archaic, dead map[string]Label, now time.Time, parent *Label) {
	for i, l := range labels {
		first := parent
		if first == nil {
			first = &labels[i]
		}
		lower := strings.ToLower(l.Name)
		switch {
		case parent == nil && l.DeleteAfter == nil: // Live label
			required[lower] = l
		case l.DeleteAfter != nil && now.After(*l.DeleteAfter):
			dead[lower] = l
		case parent != nil:
			l.parent = parent
			archaic[lower] = l
		}
		ClassifyLabels(l.Previously, required, archaic, dead, now, first)
	}
}

func SyncLabels(config Configuration, repos RepoLabels) (RepoUpdates, error) {
	// Ensure the config is valid
	if err := config.validate(); err != nil {
		return nil, fmt.Errorf("invalid config: %v", err)
	}

	// Find required, dead and archaic labels
	required := make(map[string]Label) // Must exist
	archaic := make(map[string]Label)  // Migrate
	dead := make(map[string]Label)     // Delete
	ClassifyLabels(config.Labels, required, archaic, dead, time.Now(), nil)

	var validationErrors []error
	var actions []Update
	// Process all repos
	for repo, repoLabels := range repos {
		// Convert github.Label to Label
		var labels []Label
		for _, l := range repoLabels {
			labels = append(labels, Label{Name: l.Name, Description: l.Description, Color: l.Color})
		}
		// Check for any duplicate labels
		if err := validate(labels, "", make(map[string]string)); err != nil {
			validationErrors = append(validationErrors, fmt.Errorf("invalid labels in %s: %v", repo, err))
			continue
		}
		// Create lowercase map of current labels, checking for dead labels to delete.
		current := make(map[string]Label)
		for _, l := range labels {
			lower := strings.ToLower(l.Name)
			// Should we delete this dead label?
			if _, found := dead[lower]; found {
				actions = append(actions, kill(repo, l))
			}
			current[lower] = l
		}

		var moveActions []Update // Separate list to do last
		// Look for labels to migrate
		for name, l := range archaic {
			// Does the archaic label exist?
			cur, found := current[name]
			if !found { // No
				continue
			}
			// What do we want to migrate it to?
			desired := Label{Name: l.parent.Name, Description: l.Description, Color: l.parent.Color}
			desiredName := strings.ToLower(l.parent.Name)
			// Does the new label exist?
			_, found = current[desiredName]
			if found { // Yes, migrate all these labels
				moveActions = append(moveActions, move(repo, cur, desired))
			} else { // No, rename the existing label
				actions = append(actions, rename(repo, cur, desired))
				current[desiredName] = desired
			}
		}

		// Look for missing labels
		for name, l := range required {
			cur, found := current[name]
			switch {
			case !found:
				actions = append(actions, create(repo, l))
			case l.Name != cur.Name:
				actions = append(actions, rename(repo, cur, l))
			case l.Color != cur.Color:
				actions = append(actions, change(repo, l))
			case l.Description != cur.Description:
				actions = append(actions, change(repo, l))
			}
		}

		for _, a := range moveActions {
			actions = append(actions, a)
		}
	}

	u := RepoUpdates{}
	for _, a := range actions {
		u[a.repo] = append(u[a.repo], a)
	}

	var overallErr error
	if len(validationErrors) > 0 {
		overallErr = fmt.Errorf("label validation failed: %v", validationErrors)
	}
	return u, overallErr
}

type repoUpdate struct {
	repo   string
	update Update
}

// DoUpdates iterates generated update data and adds and/or modifies labels on repositories
// Uses AddLabel GH API to add missing labels
// And UpdateLabel GH API to update color or name (name only when case differs)
func (ru RepoUpdates) DoUpdates(org string, gc client) error {
	var numUpdates int
	for _, updates := range ru {
		numUpdates += len(updates)
	}

	updateChan := make(chan repoUpdate, numUpdates)
	for repo, updates := range ru {
		logrus.WithField("org", org).WithField("repo", repo).Infof("Applying %d changes", len(updates))
		for _, item := range updates {
			updateChan <- repoUpdate{repo: repo, update: item}
		}
	}
	close(updateChan)

	wg := sync.WaitGroup{}
	wg.Add(maxConcurrentWorkers)
	errChan := make(chan error, numUpdates)
	for i := 0; i < maxConcurrentWorkers; i++ {
		go func(updates <-chan repoUpdate) {
			defer wg.Done()
			for item := range updates {
				repo := item.repo
				update := item.update
				logrus.WithField("org", org).WithField("repo", repo).WithField("why", update.Why).Debug("running update")
				switch update.Why {
				case "missing":
					err := gc.AddRepoLabel(org, repo, update.Wanted.Name, update.Wanted.Description, update.Wanted.Color)
					if err != nil {
						errChan <- err
					}
				case "change", "rename":
					err := gc.UpdateRepoLabel(org, repo, update.Current.Name, update.Wanted.Name, update.Wanted.Description, update.Wanted.Color)
					if err != nil {
						errChan <- err
					}
				case "dead":
					err := gc.DeleteRepoLabel(org, repo, update.Current.Name)
					if err != nil {
						errChan <- err
					}
				case "migrate":
					issues, err := gc.FindIssues(fmt.Sprintf("is:open repo:%s/%s label:\"%s\" -label:\"%s\"", org, repo, update.Current.Name, update.Wanted.Name), "", false)
					if err != nil {
						errChan <- err
					}
					if len(issues) == 0 {
						if err = gc.DeleteRepoLabel(org, repo, update.Current.Name); err != nil {
							errChan <- err
						}
					}
					for _, i := range issues {
						if err = gc.AddLabel(org, repo, i.Number, update.Wanted.Name); err != nil {
							errChan <- err
							continue
						}
						if err = gc.RemoveLabel(org, repo, i.Number, update.Current.Name); err != nil {
							errChan <- err
						}
					}
				default:
					errChan <- errors.New("unknown label operation: " + update.Why)
				}
			}
		}(updateChan)
	}

	wg.Wait()
	close(errChan)

	var overallErr error
	if len(errChan) > 0 {
		var updateErrs []error
		for updateErr := range errChan {
			updateErrs = append(updateErrs, updateErr)
		}
		overallErr = fmt.Errorf("failed to list labels: %v", updateErrs)
	}

	return overallErr
}

type client interface {
	AddRepoLabel(org, repo, name, description, color string) error
	UpdateRepoLabel(org, repo, currentName, newName, description, color string) error
	DeleteRepoLabel(org, repo, label string) error
	AddLabel(org, repo string, number int, label string) error
	RemoveLabel(org, repo string, number int, label string) error
	FindIssues(query, order string, ascending bool) ([]github.Issue, error)
	GetRepos(org string, isUser bool) ([]github.Repo, error)
	GetRepoLabels(string, string) ([]github.Label, error)
}

func newClient(tokenPath string, dryRun bool, hosts ...string) (client, error) {
	if tokenPath == "" {
		return nil, errors.New("--token unset")
	}
	b, err := ioutil.ReadFile(tokenPath)
	if err != nil {
		return nil, fmt.Errorf("failed to read --token=%s: %v", tokenPath, err)
	}
	oauthSecret := string(bytes.TrimSpace(b))

	if dryRun {
		return github.NewDryRunClient(oauthSecret, hosts...), nil
	}
	c := github.NewClient(oauthSecret, hosts...)
	c.Throttle(300, 100) // 300 hourly tokens, bursts of 100
	return c, nil
}

// Main function
// Typical run with production configuration should require no parameters
// It expects:
// "labels" file in "/etc/config/labels.yaml"
// github OAuth2 token in "/etc/github/oauth", this token must have write access to all org's repos
// default org is "kubernetes"
// It uses request retrying (in case of run out of GH API points)
// It took about 10 minutes to process all my 8 repos with all wanted "kubernetes" labels (70+)
// Next run takes about 22 seconds to check if all labels are correct on all repos
func main() {
	flag.Parse()
	if *debug {
		logrus.SetLevel(logrus.DebugLevel)
	}

	config, err := LoadConfig(*labelsPath)
	if err != nil {
		logrus.WithError(err).Fatalf("failed to load --config=%s", *labelsPath)
	}

	switch {
	case *action == "docs":
		if err := WriteDocs(*docsTemplate, *docsOutput, *config); err != nil {
			logrus.WithError(err).Fatalf("failed to write docs using docs-template %s to docs-output %s", *docsTemplate, *docsOutput)
		}
	case *action == "sync":
		githubClient, err := newClient(*token, !*confirm, endpoint.Strings()...)
		if err != nil {
			logrus.WithError(err).Fatal("failed to create client")
		}

		var filt filter
		switch {
		case *onlyRepos != "":
			if *skipRepos != "" {
				logrus.Fatalf("--only and --skip cannot both be set")
			}
			only := make(map[string]bool)
			for _, r := range strings.Split(*onlyRepos, ",") {
				only[strings.TrimSpace(r)] = true
			}
			filt = func(org, repo string) bool {
				_, ok := only[org+"/"+repo]
				return ok
			}
		case *skipRepos != "":
			skip := make(map[string]bool)
			for _, r := range strings.Split(*skipRepos, ",") {
				skip[strings.TrimSpace(r)] = true
			}
			filt = func(org, repo string) bool {
				_, ok := skip[org+"/"+repo]
				return !ok
			}
		default:
			filt = func(o, r string) bool {
				return true
			}
		}

		for _, org := range strings.Split(*orgs, ",") {
			org = strings.TrimSpace(org)

			if err = SyncOrg(org, githubClient, *config, filt); err != nil {
				logrus.WithError(err).Fatalf("failed to update %s", org)
			}
		}
	default:
		logrus.Fatalf("unrecognized action: %s", *action)
	}
}

type filter func(string, string) bool

func WriteDocs(template string, output string, config Configuration) error {
	labels := map[string][]Label{
		"both issues and PRs": config.LabelsByTarget(BothTarget),
		"only issues":         config.LabelsByTarget(IssueTarget),
		"only PRs":            config.LabelsByTarget(PRTarget),
	}
	if err := writeTemplate(*docsTemplate, *docsOutput, labels); err != nil {
		return err
	}
	return nil
}

func SyncOrg(org string, githubClient client, config Configuration, filt filter) error {
	logrus.WithField("org", org).Info("Reading repos")
	repos, err := LoadRepos(org, githubClient, filt)
	if err != nil {
		return err
	}

	logrus.WithField("org", org).Infof("Found %d repos", len(repos))
	currLabels, err := LoadLabels(githubClient, org, repos)
	if err != nil {
		return err
	}

	logrus.WithField("org", org).Infof("Syncing labels for %d repos", len(repos))
	updates, err := SyncLabels(config, *currLabels)
	if err != nil {
		return err
	}

	y, _ := yaml.Marshal(updates)
	logrus.Debug(string(y))

	if !*confirm {
		logrus.Infof("Running without --confirm, no mutations made")
		return nil
	}

	if err = updates.DoUpdates(org, githubClient); err != nil {
		return err
	}
	return nil
}
