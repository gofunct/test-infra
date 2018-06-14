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

package sources

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"sort"
	"time"

	"github.com/golang/glog"

	githubapi "github.com/google/go-github/github"
	"k8s.io/test-infra/mungegithub/mungers/mungerutil"
	"k8s.io/test-infra/robots/issue-creator/creator"
)

// FlakyJob is a struct that represents a single job and the flake data associated with it.
// FlakyJob implements the Issue interface so that it can be synced with github issues via the IssueCreator.
type FlakyJob struct {
	// Name is the job's name.
	Name string
	// Consistency is the percentage of builds that passed.
	Consistency *float64 `json:"consistency"`
	// FlakeCount is the number of flakes.
	FlakeCount *int `json:"flakes"`
	// FlakyTests is a map of test names to the number of times that test failed.
	// Any test that failed at least once a day for the past week on this job is included.
	FlakyTests map[string]int `json:"flakiest"`
	// testsSorted is a list of the FlakyTests test names sorted by desc. number of flakes.
	// This field is lazily populated and should be accessed via TestsSorted().
	testsSorted []string

	// reporter is a pointer to the FlakyJobReporter that created this FlakyJob.
	reporter *FlakyJobReporter
}

// FlakyJobReporter is a munger that creates github issues for the flakiest kubernetes jobs.
// The flakiest jobs are parsed from JSON generated by /test-infra/experiment/bigquery/flakes.sh
type FlakyJobReporter struct {
	flakyJobDataURL string
	syncCount       int

	creator *creator.IssueCreator
}

func init() {
	creator.RegisterSourceOrDie("flakyjob-reporter", &FlakyJobReporter{})
}

// RegisterFlags registers options for this munger; returns any that require a restart when changed.
func (fjr *FlakyJobReporter) RegisterFlags() {
	flag.StringVar(&fjr.flakyJobDataURL, "flakyjob-url", "https://storage.googleapis.com/k8s-metrics/flakes-latest.json", "The url where flaky job JSON data can be found.")
	flag.IntVar(&fjr.syncCount, "flakyjob-count", 3, "The number of flaky jobs to try to sync to github.")
}

// Issues is the main work method of FlakyJobReporter. It fetches and parses flaky job data,
// then syncs the top issues to github with the IssueCreator.
func (fjr *FlakyJobReporter) Issues(c *creator.IssueCreator) ([]creator.Issue, error) {
	fjr.creator = c
	json, err := mungerutil.ReadHTTP(fjr.flakyJobDataURL)
	if err != nil {
		return nil, err
	}

	flakyJobs, err := fjr.parseFlakyJobs(json)
	if err != nil {
		return nil, err
	}

	count := fjr.syncCount
	if len(flakyJobs) < count {
		count = len(flakyJobs)
	}
	issues := make([]creator.Issue, 0, count)
	for _, fj := range flakyJobs[0:count] {
		issues = append(issues, fj)
	}

	return issues, nil
}

// parseFlakyJobs parses JSON generated by the 'flakes' bigquery metric into a sorted slice of
// *FlakyJob.
func (fjr *FlakyJobReporter) parseFlakyJobs(jsonIn []byte) ([]*FlakyJob, error) {
	var flakeMap map[string]*FlakyJob
	err := json.Unmarshal(jsonIn, &flakeMap)
	if err != nil || flakeMap == nil {
		return nil, fmt.Errorf("error unmarshaling flaky jobs json: %v", err)
	}
	flakyJobs := make([]*FlakyJob, 0, len(flakeMap))

	for job, fj := range flakeMap {
		if job == "" {
			glog.Errorf("Flaky jobs json contained a job with an empty jobname.\n")
			continue
		}
		if fj == nil {
			glog.Errorf("Flaky jobs json has invalid data for job '%s'.\n", job)
			continue
		}
		if fj.Consistency == nil {
			glog.Errorf("Flaky jobs json has no 'consistency' field for job '%s'.\n", job)
			continue
		}
		if fj.FlakeCount == nil {
			glog.Errorf("Flaky jobs json has no 'flakes' field for job '%s'.\n", job)
			continue
		}
		if fj.FlakyTests == nil {
			glog.Errorf("Flaky jobs json has no 'flakiest' field for job '%s'.\n", job)
			continue
		}
		fj.Name = job
		fj.reporter = fjr
		flakyJobs = append(flakyJobs, fj)
	}

	sort.SliceStable(flakyJobs, func(i, j int) bool {
		if *flakyJobs[i].FlakeCount == *flakyJobs[j].FlakeCount {
			return *flakyJobs[i].Consistency < *flakyJobs[j].Consistency
		}
		return *flakyJobs[i].FlakeCount > *flakyJobs[j].FlakeCount
	})

	return flakyJobs, nil
}

// TestsSorted returns a slice of the testnames from a FlakyJob's FlakyTests map. The slice is
// sorted by descending number of failures for the tests.
func (fj *FlakyJob) TestsSorted() []string {
	if fj.testsSorted != nil {
		return fj.testsSorted
	}
	fj.testsSorted = make([]string, len(fj.FlakyTests))
	i := 0
	for test := range fj.FlakyTests {
		fj.testsSorted[i] = test
		i++
	}
	sort.SliceStable(fj.testsSorted, func(i, j int) bool {
		return fj.FlakyTests[fj.testsSorted[i]] > fj.FlakyTests[fj.testsSorted[j]]
	})
	return fj.testsSorted
}

// Title yields the initial title text of the github issue.
func (fj *FlakyJob) Title() string {
	return fmt.Sprintf("%s flaked %d times in the past week", fj.Name, *fj.FlakeCount)
}

// ID yields the string identifier that uniquely identifies this issue.
// This ID must appear in the body of the issue.
// DO NOT CHANGE how this ID is formatted or duplicate issues may be created on github.
func (fj *FlakyJob) ID() string {
	return fmt.Sprintf("Flaky Job: %s", fj.Name)
}

// Body returns the body text of the github issue and *must* contain the output of ID().
// closedIssues is a (potentially empty) slice containing all closed issues authored by this bot
// that contain ID() in their body.
// If Body returns an empty string no issue is created.
func (fj *FlakyJob) Body(closedIssues []*githubapi.Issue) string {
	// First check that the most recently closed issue (if any exist) was closed
	// at least a week ago (since that is the sliding window size used by the flake metric).
	cutoffTime := time.Now().AddDate(0, 0, -7)
	for _, closed := range closedIssues {
		if closed.ClosedAt.After(cutoffTime) {
			return ""
		}
	}

	// Print stats about the flaky job.
	var buf bytes.Buffer
	fmt.Fprintf(&buf, "### %s\n Flakes in the past week: **%d**\n Consistency: **%.2f%%**\n",
		fj.ID(), *fj.FlakeCount, *fj.Consistency*100)
	if len(fj.FlakyTests) > 0 {
		fmt.Fprint(&buf, "\n#### Flakiest tests by flake count:\n| Test | Flake Count |\n| --- | --- |\n")
		for _, testName := range fj.TestsSorted() {
			fmt.Fprintf(&buf, "| %s | %d |\n", testName, fj.FlakyTests[testName])
		}
	}
	// List previously closed issues if there are any.
	if len(closedIssues) > 0 {
		fmt.Fprint(&buf, "\n#### Previously closed issues for this job flaking:\n")
		for _, closed := range closedIssues {
			fmt.Fprintf(&buf, "#%d ", *closed.Number)
		}
		fmt.Fprint(&buf, "\n")
	}

	// Create /assign command.
	testsSorted := fj.TestsSorted()
	ownersMap := fj.reporter.creator.TestsOwners(testsSorted)
	if len(ownersMap) > 0 {
		fmt.Fprint(&buf, "\n/assign")
		for user := range ownersMap {
			fmt.Fprintf(&buf, " @%s", user)
		}
		fmt.Fprint(&buf, "\n")
	}

	// Explain why assignees were assigned and why sig labels were applied.
	fmt.Fprintf(&buf, "\n%s", fj.reporter.creator.ExplainTestAssignments(testsSorted))

	fmt.Fprintf(&buf, "\n[Flakiest Jobs](%s)\n", fj.reporter.flakyJobDataURL)
	return buf.String()
}

// Labels returns the labels to apply to the issue created for this flaky job on github.
func (fj *FlakyJob) Labels() []string {
	labels := []string{"kind/flake"}
	// get sig labels
	for sig := range fj.reporter.creator.TestsSIGs(fj.TestsSorted()) {
		labels = append(labels, "sig/"+sig)
	}
	return labels
}

// Owners returns the list of usernames to assign to this issue on github.
func (fj *FlakyJob) Owners() []string {
	// Assign owners by including a /assign command in the body instead of using Owners to set
	// assignees on the issue request. This lets prow do the assignee validation and will mention
	// the user we want to assign even if they can't be assigned.
	return nil
}

// Priority calculates and returns the priority of this issue
// The returned bool indicates if the returned priority is valid and can be used
func (fj *FlakyJob) Priority() (string, bool) {
	// TODO: implement priority calculations later
	return "", false
}
