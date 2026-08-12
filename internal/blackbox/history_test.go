package blackbox

import (
	"strings"
	"testing"
)

const validHistory = `{"version":1,"id":"history-help","category":"ux","title":"Help omitted a flag","status":"resolved","symptom":"The command summary omitted a supported flag.","expected":"The summary names supported resources.","public_surfaces":["workspace resources --help"],"sources":[{"system":"tracker","reference":"issue-123"}]}`

func TestDecodeHistory(t *testing.T) {
	history, err := DecodeHistory(strings.NewReader(validHistory + "\n"))
	if err != nil || len(history) != 1 || history[0].Sources[0].Reference != "issue-123" {
		t.Fatalf("DecodeHistory = %#v, %v", history, err)
	}
	if _, err := DecodeHistory(strings.NewReader(validHistory + "\n" + validHistory)); err == nil {
		t.Fatal("duplicate history accepted")
	}
	if _, err := DecodeHistory(nil); err == nil {
		t.Fatal("nil history accepted")
	}
	if _, err := DecodeHistory(strings.NewReader(`{"unknown":true}`)); err == nil {
		t.Fatal("unknown history fields accepted")
	}
	if _, err := DecodeHistory(strings.NewReader(`{"version":1}`)); err == nil {
		t.Fatal("invalid history record accepted")
	}
	if _, err := DecodeHistory(strings.NewReader(strings.Repeat("x", MaxDocumentBytes+1))); err == nil {
		t.Fatal("oversized history accepted")
	}
}

func TestHistoryValidationRejectsInvalidRecords(t *testing.T) {
	base := HistoryIssue{
		Version: Version, ID: "history-help", Category: CategoryUX, Title: "example title",
		Status: HistoryResolved, Symptom: "example symptom", Expected: "expected behavior",
		PublicSurfaces: []string{"daft --help"}, Sources: []HistorySource{{System: "tracker", Reference: "123"}},
	}
	cases := []struct {
		name   string
		mutate func(*HistoryIssue)
	}{
		{"version", func(i *HistoryIssue) { i.Version = 2 }},
		{"id", func(i *HistoryIssue) { i.ID = "" }},
		{"category", func(i *HistoryIssue) { i.Category = "other" }},
		{"status", func(i *HistoryIssue) { i.Status = "unknown" }},
		{"text", func(i *HistoryIssue) { i.Symptom = "" }},
		{"title", func(i *HistoryIssue) { i.Title = "" }},
		{"expected", func(i *HistoryIssue) { i.Expected = "" }},
		{"surface", func(i *HistoryIssue) { i.PublicSurfaces = nil }},
		{"empty surface", func(i *HistoryIssue) { i.PublicSurfaces[0] = "" }},
		{"many surfaces", func(i *HistoryIssue) { i.PublicSurfaces = make([]string, MaxAssertions+1) }},
		{"source", func(i *HistoryIssue) { i.Sources = nil }},
		{"source system", func(i *HistoryIssue) { i.Sources[0].System = "Bad Tracker" }},
		{"source reference", func(i *HistoryIssue) { i.Sources[0].Reference = "" }},
		{"many sources", func(i *HistoryIssue) { i.Sources = make([]HistorySource, MaxHistorySources+1) }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			issue := base
			issue.PublicSurfaces = append([]string(nil), base.PublicSurfaces...)
			issue.Sources = append([]HistorySource(nil), base.Sources...)
			tc.mutate(&issue)
			if err := issue.Validate(); err == nil {
				t.Fatal("invalid history accepted")
			}
		})
	}
}
