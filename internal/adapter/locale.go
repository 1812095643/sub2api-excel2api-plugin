package adapter

import (
	"encoding/xml"
	"io"
	"strconv"
	"strings"
	"time"

	pluginv1 "github.com/Wei-Shaw/sub2api/pkg/pluginapi/v1"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

type headerValues = pluginv1.HeaderValues

type timezoneReplacement struct {
	start int
	end   int
	value string
}

type timezoneNormalization struct {
	TargetTimezone string   `json:"target_timezone"`
	Before         []string `json:"timezone_before,omitempty"`
	After          []string `json:"timezone_after,omitempty"`
	WebBefore      []string `json:"web_search_timezone_before,omitempty"`
	WebAfter       []string `json:"web_search_timezone_after,omitempty"`
	Matched        int      `json:"matched_count"`
	Replaced       int      `json:"replaced_count"`
	Reason         string   `json:"reason"`
}

func rewriteEnvironment(text, timezone, date string) (string, string, string, bool, bool) {
	trimmed := strings.TrimSpace(text)
	if !strings.HasPrefix(trimmed, "<environment_context>") || !strings.HasSuffix(trimmed, "</environment_context>") {
		return text, "", "", false, false
	}
	decoder := xml.NewDecoder(strings.NewReader(text))
	depth := 0
	activeTag := ""
	activeStart := 0
	before, after := "", ""
	var replacements []timezoneReplacement
	for {
		start := int(decoder.InputOffset())
		token, err := decoder.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return text, "", "", false, false
		}
		switch element := token.(type) {
		case xml.StartElement:
			depth++
			if depth == 1 && element.Name.Local != "environment_context" {
				return text, "", "", false, false
			}
			if depth == 2 && (element.Name.Local == "timezone" || element.Name.Local == "current_date") {
				if activeTag != "" || (element.Name.Local == "timezone" && before != "") {
					return text, "", "", false, false
				}
				activeTag = element.Name.Local
				activeStart = int(decoder.InputOffset())
				if activeStart >= 2 && text[activeStart-2:activeStart] == "/>" {
					return text, "", "", false, false
				}
			}
			if depth > 2 && activeTag != "" {
				return text, "", "", false, false
			}
		case xml.EndElement:
			if depth == 2 && activeTag == element.Name.Local {
				original := text[activeStart:start]
				if strings.Contains(original, "<") {
					return text, "", "", false, false
				}
				left := len(original) - len(strings.TrimLeft(original, " \t\n\r"))
				right := len(strings.TrimRight(original, " \t\n\r"))
				value := timezone
				if activeTag == "current_date" {
					value = date
				} else {
					before = strings.TrimSpace(original)
					if len(before) > 128 {
						before = before[:128]
					}
					after = timezone
				}
				if right >= left {
					replacement := original[:left] + value + original[right:]
					if replacement != original {
						replacements = append(replacements, timezoneReplacement{activeStart, start, replacement})
					}
				}
				activeTag = ""
			}
			depth--
			if depth < 0 {
				return text, "", "", false, false
			}
		}
	}
	if depth != 0 {
		return text, "", "", false, false
	}
	for index := len(replacements) - 1; index >= 0; index-- {
		replacement := replacements[index]
		text = text[:replacement.start] + replacement.value + text[replacement.end:]
	}
	return text, before, after, true, before != "" && before != timezone
}

func normalizeRequestLocale(body []byte, accountID int64, timezone string, now time.Time) ([]byte, timezoneNormalization) {
	result := timezoneNormalization{TargetTimezone: timezone, Reason: "no_marked_context"}
	location, err := time.LoadLocation(timezone)
	if err != nil {
		location, _ = time.LoadLocation("Asia/Singapore")
		result.TargetTimezone = "Asia/Singapore"
	}
	date := now.In(location).Format("2006-01-02")
	input := gjson.GetBytes(body, "input")
	invalidXML := false
	if input.IsArray() {
		for inputIndex, item := range input.Array() {
			if item.Get("role").String() != "user" {
				continue
			}
			kinds := item.Get("internal_chat_message_metadata_passthrough.content_item_kinds")
			content := item.Get("content")
			if !kinds.IsArray() || !content.IsArray() {
				continue
			}
			kindValues := kinds.Array()
			for contentIndex, part := range content.Array() {
				if contentIndex >= len(kindValues) || kindValues[contentIndex].String() != "environments.environment_context" || part.Get("type").String() != "input_text" {
					continue
				}
				text := part.Get("text")
				if text.Type != gjson.String {
					continue
				}
				result.Matched++
				next, prior, current, valid, changed := rewriteEnvironment(text.String(), result.TargetTimezone, date)
				if prior != "" {
					result.Before = append(result.Before, prior)
				}
				if current != "" {
					result.After = append(result.After, current)
				}
				if !valid {
					invalidXML = true
					continue
				}
				if next != text.String() {
					updated, setErr := sjson.SetBytes(body, "input."+strconv.Itoa(inputIndex)+".content."+strconv.Itoa(contentIndex)+".text", next)
					if setErr == nil {
						body = updated
						if changed {
							result.Replaced++
						}
					} else {
						invalidXML = true
					}
				}
			}
		}
	}
	tools := gjson.GetBytes(body, "tools")
	if tools.IsArray() {
		for index, tool := range tools.Array() {
			kind := tool.Get("type").String()
			if kind != "web_search" && !strings.HasPrefix(kind, "web_search_") {
				continue
			}
			value := tool.Get("user_location.timezone")
			if value.Type != gjson.String {
				continue
			}
			prior := value.String()
			result.WebBefore = append(result.WebBefore, prior)
			current := prior
			if prior != result.TargetTimezone {
				if updated, setErr := sjson.SetBytes(body, "tools."+strconv.Itoa(index)+".user_location.timezone", result.TargetTimezone); setErr == nil {
					body = updated
					current = result.TargetTimezone
					result.Replaced++
				}
			}
			result.WebAfter = append(result.WebAfter, current)
		}
	}
	if result.Replaced > 0 {
		result.Reason = "replaced"
	} else if invalidXML {
		result.Reason = "invalid_xml"
	} else if result.Matched > 0 {
		result.Reason = "no_timezone"
		for _, value := range result.Before {
			if value != result.TargetTimezone {
				result.Reason = "already_target"
				break
			}
		}
	}
	_ = accountID
	return body, result
}

func normalizeAcceptLanguage(headers map[string]*headerValues) bool {
	if headers == nil {
		return false
	}
	found := false
	for name := range headers {
		if strings.EqualFold(name, "Accept-Language") {
			found = true
			delete(headers, name)
		}
	}
	if found {
		headers["Accept-Language"] = &headerValues{Values: []string{"en-US,en;q=0.9"}}
	}
	return found
}
