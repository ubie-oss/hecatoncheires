package model

// CaseSummaryStyle selects how the "created a case" summary posted to Slack in
// a thread-mode workspace is rendered.
//
//   - CaseSummaryFull (default): a header, the title, description, populated
//     custom fields, board status, and a web-UI link.
//   - CaseSummaryLink: a single line, the header followed by the case title
//     linked to the web UI.
//
// It is only meaningful when CaseMode is thread; channel-mode workspaces never
// post this summary.
type CaseSummaryStyle string

const (
	// CaseSummaryFull is the default: the full Block Kit summary.
	CaseSummaryFull CaseSummaryStyle = "full"
	// CaseSummaryLink renders the summary as one line linking to the case.
	CaseSummaryLink CaseSummaryStyle = "link"
)

// IsValid reports whether the style is a recognised value. The empty string is
// not valid here; callers normalise empty to CaseSummaryFull before use.
func (s CaseSummaryStyle) IsValid() bool {
	switch s {
	case CaseSummaryFull, CaseSummaryLink:
		return true
	default:
		return false
	}
}

// Normalize maps the empty string to the default CaseSummaryFull.
func (s CaseSummaryStyle) Normalize() CaseSummaryStyle {
	if s == "" {
		return CaseSummaryFull
	}
	return s
}

// IsLink reports whether the summary is rendered as a single linked line.
func (s CaseSummaryStyle) IsLink() bool {
	return s.Normalize() == CaseSummaryLink
}
