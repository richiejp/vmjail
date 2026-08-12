package blackbox

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
)

const MaxHistorySources = 8

type HistoryStatus string

const (
	HistoryOpen     HistoryStatus = "open"
	HistoryResolved HistoryStatus = "resolved"
)

// HistoryIssue is a tracker-independent, source-blind account of prior public
// behaviour. Source identifies provenance without embedding tracker-specific
// fields or internal diagnosis in the record.
type HistoryIssue struct {
	Version        int             `json:"version"`
	ID             string          `json:"id"`
	Category       Category        `json:"category"`
	Title          string          `json:"title"`
	Status         HistoryStatus   `json:"status"`
	Symptom        string          `json:"symptom"`
	Expected       string          `json:"expected"`
	PublicSurfaces []string        `json:"public_surfaces"`
	Sources        []HistorySource `json:"sources"`
}

type HistorySource struct {
	System    string `json:"system"`
	Reference string `json:"reference"`
}

func DecodeHistory(r io.Reader) ([]HistoryIssue, error) {
	if r == nil {
		return nil, errors.New("blackbox: history reader is required")
	}
	scanner := bufio.NewScanner(io.LimitReader(r, MaxDocumentBytes+1))
	scanner.Buffer(make([]byte, 4096), MaxDocumentBytes+1)
	history := make([]HistoryIssue, 0)
	seen := map[string]struct{}{}
	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}
		if len(history) == MaxIssues {
			return nil, fmt.Errorf("blackbox: history exceeds %d records", MaxIssues)
		}
		var issue HistoryIssue
		if err := decodeStrict(bytes.NewReader(line), &issue); err != nil {
			return nil, fmt.Errorf("blackbox: history line %d: %w", len(history)+1, err)
		}
		if err := issue.Validate(); err != nil {
			return nil, fmt.Errorf("blackbox: history line %d: %w", len(history)+1, err)
		}
		if _, duplicate := seen[issue.ID]; duplicate {
			return nil, fmt.Errorf("blackbox: duplicate history issue %q", issue.ID)
		}
		seen[issue.ID] = struct{}{}
		history = append(history, issue)
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("blackbox: read history: %w", err)
	}
	return history, nil
}

func (i HistoryIssue) Validate() error {
	if i.Version != Version {
		return fmt.Errorf("history version must be %d", Version)
	}
	if !identifierPattern.MatchString(i.ID) {
		return errors.New("history id must be a lowercase identifier")
	}
	if !slices.Contains([]Category{CategoryCorrectness, CategoryUX, CategoryPerformance}, i.Category) {
		return errors.New("category must be correctness, ux, or performance")
	}
	if !slices.Contains([]HistoryStatus{HistoryOpen, HistoryResolved}, i.Status) {
		return errors.New("history status must be open or resolved")
	}
	for label, value := range map[string]string{titleField: i.Title, "symptom": i.Symptom, expectedField: i.Expected} {
		if strings.TrimSpace(value) == "" || len(value) > MaxTextBytes {
			return fmt.Errorf("%s must be non-empty and at most %d bytes", label, MaxTextBytes)
		}
	}
	if len(i.PublicSurfaces) == 0 || len(i.PublicSurfaces) > MaxAssertions {
		return fmt.Errorf("public_surfaces must contain 1..%d entries", MaxAssertions)
	}
	for _, surface := range i.PublicSurfaces {
		if strings.TrimSpace(surface) == "" || len(surface) > MaxTextBytes {
			return errors.New("public surfaces must be non-empty and bounded")
		}
	}
	if len(i.Sources) == 0 || len(i.Sources) > MaxHistorySources {
		return fmt.Errorf("sources must contain 1..%d entries", MaxHistorySources)
	}
	for _, source := range i.Sources {
		if !identifierPattern.MatchString(source.System) || strings.TrimSpace(source.Reference) == "" || len(source.Reference) > MaxTextBytes {
			return errors.New("history sources require a lowercase system and bounded reference")
		}
	}
	return nil
}
