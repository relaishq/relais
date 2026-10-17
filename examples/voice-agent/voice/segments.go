package voice

import (
	"strings"
	"unicode"
)

const (
	MaxSegments     = 8
	MaxReplyBytes   = 512
	MaxSegmentRunes = 64
	minSegmentRunes = 24
	MaxSegmentBytes = 64 << 10
)

// Segments is a pure function of the saved reply. Sentence punctuation closes a
// piece; a short piece waits for the next sentence. Oversized sentences split
// at whitespace, or at a rune boundary when no whitespace fits. Saved replies
// are limited to 512 JSON bytes, so eight 64-rune segments always fit.
func Segments(reply string) []string {
	reply = strings.TrimSpace(boundedText(reply, MaxReplyBytes))
	var result []string
	for reply != "" && len(result) < MaxSegments {
		runes := []rune(reply)
		end := len(runes)
		for i, r := range runes {
			if i+1 >= minSegmentRunes && strings.ContainsRune(".!?。！？", r) {
				end = i + 1
				break
			}
		}
		if end > MaxSegmentRunes {
			end = MaxSegmentRunes
			for i := end - 1; i >= minSegmentRunes; i-- {
				if unicode.IsSpace(runes[i]) {
					end = i
					break
				}
			}
		}
		// Reserve enough capacity for all remaining saved text, even when
		// many tiny sentences or inconvenient word boundaries would use slots.
		required := len(runes) - (MaxSegments-len(result)-1)*MaxSegmentRunes
		if end < required {
			end = required
		}
		piece := strings.TrimSpace(string(runes[:end]))
		if piece != "" {
			result = append(result, piece)
		}
		reply = strings.TrimSpace(string(runes[end:]))
	}
	return result
}
