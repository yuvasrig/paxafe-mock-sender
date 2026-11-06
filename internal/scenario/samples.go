package scenario

import (
	"embed"
	"encoding/json"
	"fmt"
	"sort"
)

//go:embed data/samples/*.json data/expectations.json
var dataFS embed.FS

type sampleExpectation struct {
	Index  int      `json:"index"`
	Name   string   `json:"name"`
	HTTP   int      `json:"http"`
	Status string   `json:"status"`
	Flags  []string `json:"flags"`
	Errors []string `json:"errors"`
}

func init() {
	register(Scenario{
		ID:    "samples",
		Title: "Provided samples",
		Description: "The 13 sample payloads from the exercise, in order, each checked against its expected status and flags. " +
			"Expected results assume a tenant that has not seen the samples; on a re-run, accepted and flagged samples answer \"duplicate\".",
		Build: func(Params) []Step { return mustSamples() },
	})
}

func mustSamples() []Step {
	steps, err := Samples()
	if err != nil {
		panic(err) // the data is embedded at build time; a failure here is a build bug, caught by tests
	}
	return steps
}

// Samples returns the 13 provided samples with their expectations.
func Samples() ([]Step, error) {
	raw, err := dataFS.ReadFile("data/expectations.json")
	if err != nil {
		return nil, err
	}
	var exps []sampleExpectation
	if err := json.Unmarshal(raw, &exps); err != nil {
		return nil, fmt.Errorf("decode expectations: %w", err)
	}
	sort.Slice(exps, func(i, j int) bool { return exps[i].Index < exps[j].Index })

	steps := make([]Step, 0, len(exps))
	for _, e := range exps {
		body, err := dataFS.ReadFile(fmt.Sprintf("data/samples/%02d.json", e.Index))
		if err != nil {
			return nil, err
		}
		statuses := []string{e.Status}
		if e.Status == "accepted" || e.Status == "flagged" {
			statuses = append(statuses, "duplicate") // the same sample sent again
		}
		steps = append(steps, Step{
			Name: fmt.Sprintf("#%d %s", e.Index, e.Name),
			Body: body,
			Expect: Expect{
				HTTP: []int{e.HTTP}, Status: statuses, Flags: e.Flags, ExactFlags: true,
				Ack: true, Rejection: e.HTTP == 422 || e.HTTP == 400,
				Note: sampleNote(e),
			},
		})
	}
	return steps, nil
}

func sampleNote(e sampleExpectation) string {
	switch e.Index {
	case 1, 4, 8:
		return "same device and time as #0, different content: first kept, this one stored as a conflict"
	case 9:
		return "AccountId 999 is unmapped: stored under common-pool"
	case 11:
		return "EntryTimeUtc is 5 h off the epoch"
	case 12:
		return "unknown DeviceName and no DeviceId"
	}
	return ""
}
