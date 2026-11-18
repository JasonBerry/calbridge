package translate

import (
	"fmt"
	"html"
	"regexp"
	"strings"

	calendar "google.golang.org/api/calendar/v3"
)

// description assembles DESCRIPTION: conference block, Google description
// (HTML converted to text), footer (§7.2, §7.5).
func description(ev *calendar.Event) string {
	var parts []string
	if b := conferenceBlock(ev); b != "" {
		parts = append(parts, b)
	}
	if d := strings.TrimSpace(HTMLToText(ev.Description)); d != "" {
		parts = append(parts, d)
	}
	parts = append(parts, Footer)
	return strings.Join(parts, "\n\n")
}

// videoURL is the URL property: hangoutLink, else the first video entry point.
func videoURL(ev *calendar.Event) string {
	if ev.HangoutLink != "" {
		return ev.HangoutLink
	}
	if ev.ConferenceData != nil {
		for _, ep := range ev.ConferenceData.EntryPoints {
			if ep != nil && ep.EntryPointType == "video" && ep.Uri != "" {
				return ep.Uri
			}
		}
	}
	return ""
}

// conferenceBlock renders video, phone and "more" entry points in that order
// followed by a separator line; it falls back to hangoutLink.
func conferenceBlock(ev *calendar.Event) string {
	var lines []string
	name := "video call"
	if cd := ev.ConferenceData; cd != nil {
		if cd.ConferenceSolution != nil && cd.ConferenceSolution.Name != "" {
			name = cd.ConferenceSolution.Name
		}
		for _, kind := range []string{"video", "phone", "more"} {
			for _, ep := range cd.EntryPoints {
				if ep == nil || ep.EntryPointType != kind || ep.Uri == "" {
					continue
				}
				switch kind {
				case "video":
					lines = append(lines, fmt.Sprintf("Join %s: %s", name, ep.Uri))
				case "phone":
					num := ep.Label
					if num == "" {
						num = strings.TrimPrefix(ep.Uri, "tel:")
					}
					line := "Dial-in: " + num
					if ep.Pin != "" {
						line += " PIN: " + ep.Pin + "#"
					}
					lines = append(lines, line)
				case "more":
					lines = append(lines, "More phone numbers: "+ep.Uri)
				}
			}
		}
	}
	if len(lines) == 0 && ev.HangoutLink != "" {
		lines = append(lines, "Join Google Meet: "+ev.HangoutLink)
	}
	if len(lines) == 0 {
		return ""
	}
	return strings.Join(lines, "\n") + "\n—"
}

var (
	reHTMLHint   = regexp.MustCompile(`(?i)<(br|p|div|a|b|i|u|span|ul|ol|li|strong|em|h[1-6])\b[^>]*>|&[a-z]+;|&#[0-9]+;`)
	reBreak      = regexp.MustCompile(`(?i)<br\s*/?>`)
	reBlockEnd   = regexp.MustCompile(`(?i)</(p|div|h[1-6]|ul|ol|tr)\s*>`)
	reListItem   = regexp.MustCompile(`(?i)<li\b[^>]*>`)
	reListEnd    = regexp.MustCompile(`(?i)</li\s*>`)
	reAnchor     = regexp.MustCompile(`(?is)<a\b[^>]*?\bhref\s*=\s*("([^"]*)"|'([^']*)')[^>]*>(.*?)</a\s*>`)
	reTag        = regexp.MustCompile(`(?s)<[^>]*>`)
	reBlankLines = regexp.MustCompile(`\n{3,}`)
	reTrailingWS = regexp.MustCompile(`[ \t]+\n`)
)

// HTMLToText converts Google's HTML descriptions to plain text: line breaks
// for <br> and block ends, bullets for <li>, "text (url)" for links whose
// text differs from the URL, tags stripped, entities unescaped. Plain text
// passes through unchanged apart from newline normalisation.
func HTMLToText(s string) string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	if !reHTMLHint.MatchString(s) {
		return s
	}
	s = strings.ReplaceAll(s, "\n", " ") // HTML whitespace is not significant
	s = reAnchor.ReplaceAllStringFunc(s, func(m string) string {
		sub := reAnchor.FindStringSubmatch(m)
		href := sub[2] + sub[3]
		text := strings.TrimSpace(html.UnescapeString(reTag.ReplaceAllString(sub[4], "")))
		href = html.UnescapeString(href)
		switch {
		case text == "":
			return href
		case href == "" || text == href || strings.TrimPrefix(href, "mailto:") == text:
			return text
		}
		return text + " (" + href + ")"
	})
	s = reBreak.ReplaceAllString(s, "\n")
	s = reListItem.ReplaceAllString(s, "\n• ")
	s = reListEnd.ReplaceAllString(s, "")
	s = reBlockEnd.ReplaceAllString(s, "\n")
	s = reTag.ReplaceAllString(s, "")
	s = html.UnescapeString(s)
	s = strings.ReplaceAll(s, " ", " ")
	s = reTrailingWS.ReplaceAllString(s, "\n")
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		lines[i] = strings.TrimSpace(l)
	}
	s = strings.Join(lines, "\n")
	s = reBlankLines.ReplaceAllString(s, "\n\n")
	return strings.TrimSpace(s)
}
