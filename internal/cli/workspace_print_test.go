package cli

import (
	"bytes"
	"strings"
	"testing"

	workspacev1 "github.com/localai-org/darkaifactory/internal/genpb/workspace/v1"
	"github.com/localai-org/darkaifactory/internal/workspace"
)

// White-box test: pinning the printChange formatting + entryKindLabel
// branches in isolation, since the real-server diff tests can't
// trigger the default case nor exercise every kind transition cheaply.
func TestPrintChangeFormatting(t *testing.T) {
	cases := []struct {
		name       string
		change     workspace.Change
		detail     bool
		wantSubstr string
	}{
		{
			name:       "added_terse",
			change:     workspace.Change{Path: "a", Kind: workspace.Added},
			wantSubstr: "+ a\n",
		},
		{
			name:       "removed_terse",
			change:     workspace.Change{Path: "r", Kind: workspace.Removed},
			wantSubstr: "- r\n",
		},
		{
			name:       "modified_detail_no_extras",
			change:     workspace.Change{Path: "m", Kind: workspace.Modified},
			detail:     true,
			wantSubstr: "~ m\n",
		},
		{
			name: "mode_detail",
			change: workspace.Change{
				Path: "p", Kind: workspace.ModeChanged,
				From: &workspacev1.FileEntry{Mode: 0o600},
				To:   &workspacev1.FileEntry{Mode: 0o755},
			},
			detail:     true,
			wantSubstr: "m p  mode 0600 -> 0755\n",
		},
		{
			name: "type_detail",
			change: workspace.Change{
				Path: "p", Kind: workspace.TypeChanged,
				From: &workspacev1.FileEntry{Kind: workspacev1.EntryKind_ENTRY_KIND_FILE},
				To:   &workspacev1.FileEntry{Kind: workspacev1.EntryKind_ENTRY_KIND_SYMLINK},
			},
			detail:     true,
			wantSubstr: "t p  file -> symlink\n",
		},
		{
			name: "type_dir_to_unspec_detail",
			change: workspace.Change{
				Path: "p", Kind: workspace.TypeChanged,
				From: &workspacev1.FileEntry{Kind: workspacev1.EntryKind_ENTRY_KIND_DIR},
				To:   &workspacev1.FileEntry{Kind: workspacev1.EntryKind_ENTRY_KIND_UNSPECIFIED},
			},
			detail:     true,
			wantSubstr: "t p  dir -> ?\n",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			printChange(&buf, tc.change, tc.detail)
			if !strings.Contains(buf.String(), tc.wantSubstr) {
				t.Errorf("got %q, want substring %q", buf.String(), tc.wantSubstr)
			}
		})
	}
}
