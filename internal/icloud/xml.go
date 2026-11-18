package icloud

import (
	"encoding/xml"
	"fmt"
	"net/http"
	"strconv"
	"strings"
)

// Request bodies. Kept as literal XML so captures are easy to compare.

const propfindPrincipal = `<?xml version="1.0" encoding="utf-8"?>
<d:propfind xmlns:d="DAV:"><d:prop><d:current-user-principal/></d:prop></d:propfind>`

const propfindHome = `<?xml version="1.0" encoding="utf-8"?>
<d:propfind xmlns:d="DAV:" xmlns:c="urn:ietf:params:xml:ns:caldav">
 <d:prop>
  <c:calendar-home-set/>
  <c:calendar-user-address-set/>
  <c:schedule-inbox-URL/>
  <c:schedule-outbox-URL/>
 </d:prop>
</d:propfind>`

const propfindCalendars = `<?xml version="1.0" encoding="utf-8"?>
<d:propfind xmlns:d="DAV:" xmlns:c="urn:ietf:params:xml:ns:caldav" xmlns:cs="http://calendarserver.org/ns/">
 <d:prop>
  <d:displayname/>
  <d:resourcetype/>
  <c:supported-calendar-component-set/>
  <cs:getctag/>
  <d:sync-token/>
  <d:supported-report-set/>
  <d:current-user-privilege-set/>
 </d:prop>
</d:propfind>`

const propfindCTag = `<?xml version="1.0" encoding="utf-8"?>
<d:propfind xmlns:d="DAV:" xmlns:cs="http://calendarserver.org/ns/"><d:prop><cs:getctag/><d:sync-token/></d:prop></d:propfind>`

const propfindETags = `<?xml version="1.0" encoding="utf-8"?>
<d:propfind xmlns:d="DAV:"><d:prop><d:getetag/><d:resourcetype/></d:prop></d:propfind>`

func syncCollectionBody(token string) string {
	return `<?xml version="1.0" encoding="utf-8"?>
<d:sync-collection xmlns:d="DAV:">
 <d:sync-token>` + xmlEscape(token) + `</d:sync-token>
 <d:sync-level>1</d:sync-level>
 <d:prop><d:getetag/></d:prop>
</d:sync-collection>`
}

func multigetBody(hrefs []string) string {
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="utf-8"?>
<c:calendar-multiget xmlns:d="DAV:" xmlns:c="urn:ietf:params:xml:ns:caldav">
 <d:prop><d:getetag/><c:calendar-data/></d:prop>
`)
	for _, h := range hrefs {
		b.WriteString(" <d:href>" + xmlEscape(h) + "</d:href>\n")
	}
	b.WriteString("</c:calendar-multiget>")
	return b.String()
}

func uidQueryBody(uid string) string {
	return `<?xml version="1.0" encoding="utf-8"?>
<c:calendar-query xmlns:d="DAV:" xmlns:c="urn:ietf:params:xml:ns:caldav">
 <d:prop><d:getetag/><c:calendar-data/></d:prop>
 <c:filter>
  <c:comp-filter name="VCALENDAR">
   <c:comp-filter name="VEVENT">
    <c:prop-filter name="UID">
     <c:text-match collation="i;octet">` + xmlEscape(uid) + `</c:text-match>
    </c:prop-filter>
   </c:comp-filter>
  </c:comp-filter>
 </c:filter>
</c:calendar-query>`
}

func xmlEscape(s string) string {
	var b strings.Builder
	_ = xml.EscapeText(&b, []byte(s))
	return b.String()
}

// Multistatus parsing.

type multistatus struct {
	XMLName   xml.Name     `xml:"DAV: multistatus"`
	Responses []msResponse `xml:"DAV: response"`
	SyncToken string       `xml:"DAV: sync-token"`
}

type msResponse struct {
	Hrefs     []string     `xml:"DAV: href"`
	Status    string       `xml:"DAV: status"`
	Propstats []msPropstat `xml:"DAV: propstat"`
	Error     *anyChildren `xml:"DAV: error"`
}

type msPropstat struct {
	Prop   davProp `xml:"DAV: prop"`
	Status string  `xml:"DAV: status"`
}

type hrefList struct {
	Hrefs []string `xml:"DAV: href"`
}

type anyChildren struct {
	Children []struct {
		XMLName xml.Name
	} `xml:",any"`
}

func (a *anyChildren) names() []string {
	if a == nil {
		return nil
	}
	var out []string
	for _, c := range a.Children {
		out = append(out, c.XMLName.Local)
	}
	return out
}

type davProp struct {
	CurrentUserPrincipal *hrefList    `xml:"DAV: current-user-principal"`
	CalendarHomeSet      *hrefList    `xml:"urn:ietf:params:xml:ns:caldav calendar-home-set"`
	AddressSet           *hrefList    `xml:"urn:ietf:params:xml:ns:caldav calendar-user-address-set"`
	ScheduleInbox        *hrefList    `xml:"urn:ietf:params:xml:ns:caldav schedule-inbox-URL"`
	ScheduleOutbox       *hrefList    `xml:"urn:ietf:params:xml:ns:caldav schedule-outbox-URL"`
	DisplayName          string       `xml:"DAV: displayname"`
	ResourceType         *anyChildren `xml:"DAV: resourcetype"`
	SupportedComponents  *struct {
		Comps []struct {
			Name string `xml:"name,attr"`
		} `xml:"urn:ietf:params:xml:ns:caldav comp"`
	} `xml:"urn:ietf:params:xml:ns:caldav supported-calendar-component-set"`
	CTag               string `xml:"http://calendarserver.org/ns/ getctag"`
	SyncToken          string `xml:"DAV: sync-token"`
	SupportedReportSet *struct {
		Reports []struct {
			Report anyChildren `xml:"DAV: report"`
		} `xml:"DAV: supported-report"`
	} `xml:"DAV: supported-report-set"`
	PrivilegeSet *struct {
		Privileges []anyChildren `xml:"DAV: privilege"`
	} `xml:"DAV: current-user-privilege-set"`
	GetETag      string `xml:"DAV: getetag"`
	CalendarData string `xml:"urn:ietf:params:xml:ns:caldav calendar-data"`
}

func parseMultistatus(b []byte) (*multistatus, error) {
	var ms multistatus
	if err := xml.Unmarshal(b, &ms); err != nil {
		return nil, fmt.Errorf("caldav: parse multistatus: %w", err)
	}
	return &ms, nil
}

// statusCode parses "HTTP/1.1 200 OK" → 200; 0 if absent or malformed.
func statusCode(s string) int {
	f := strings.Fields(s)
	if len(f) < 2 {
		return 0
	}
	n, err := strconv.Atoi(f[1])
	if err != nil {
		return 0
	}
	return n
}

// okProp merges the props of all 200 propstats of a response.
func (r *msResponse) okProp() davProp {
	var out davProp
	for _, ps := range r.Propstats {
		if c := statusCode(ps.Status); c != 0 && c != http.StatusOK {
			continue
		}
		p := ps.Prop
		mergeProp(&out, &p)
	}
	return out
}

func mergeProp(dst, src *davProp) {
	if src.CurrentUserPrincipal != nil {
		dst.CurrentUserPrincipal = src.CurrentUserPrincipal
	}
	if src.CalendarHomeSet != nil {
		dst.CalendarHomeSet = src.CalendarHomeSet
	}
	if src.AddressSet != nil {
		dst.AddressSet = src.AddressSet
	}
	if src.ScheduleInbox != nil {
		dst.ScheduleInbox = src.ScheduleInbox
	}
	if src.ScheduleOutbox != nil {
		dst.ScheduleOutbox = src.ScheduleOutbox
	}
	if src.DisplayName != "" {
		dst.DisplayName = src.DisplayName
	}
	if src.ResourceType != nil {
		dst.ResourceType = src.ResourceType
	}
	if src.SupportedComponents != nil {
		dst.SupportedComponents = src.SupportedComponents
	}
	if src.CTag != "" {
		dst.CTag = src.CTag
	}
	if src.SyncToken != "" {
		dst.SyncToken = src.SyncToken
	}
	if src.SupportedReportSet != nil {
		dst.SupportedReportSet = src.SupportedReportSet
	}
	if src.PrivilegeSet != nil {
		dst.PrivilegeSet = src.PrivilegeSet
	}
	if src.GetETag != "" {
		dst.GetETag = src.GetETag
	}
	if src.CalendarData != "" {
		dst.CalendarData = src.CalendarData
	}
}

func (r *msResponse) href() string {
	if len(r.Hrefs) == 0 {
		return ""
	}
	return strings.TrimSpace(r.Hrefs[0])
}

func firstHref(h *hrefList) string {
	if h == nil || len(h.Hrefs) == 0 {
		return ""
	}
	return strings.TrimSpace(h.Hrefs[0])
}

func (p *davProp) isCollection() bool {
	for _, n := range p.ResourceType.names() {
		if n == "collection" {
			return true
		}
	}
	return false
}

func (p *davProp) isCalendar() bool {
	for _, n := range p.ResourceType.names() {
		if n == "calendar" {
			return true
		}
	}
	return false
}
