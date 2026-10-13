// SPDX-License-Identifier: Apache-2.0

package transcribe

import (
	"strconv"
	"strings"
	"testing"
	"unicode"
)

// Scoring for the language-drift set (issue #127, testdata/langdrift).
//
// A reference is scored against the recogniser's text after both are brought
// to one normal form, so that a number written as "19", "十九", or "nineteen"
// costs nothing and only a wrong number does:
//
//   - everything is lower-cased and stripped of punctuation and spaces
//     ("Wi-Fi" and "WiFi" are both "wifi", "month's" is "months");
//   - zh: a run of numerals (Arabic digits and 零〇一二三四五六七八九两十百千万亿)
//     becomes one Arabic number. With a unit (十百千万亿) it is read as a whole
//     number (三千五百二十 is 3520), without one as digits read one by one
//     (四四七一 is 4471). Spaces are dropped first, so "4 4 7 1" is 4471;
//   - en: runs of number words become digits the same way: single digits read
//     one by one are concatenated ("four four seven one" is 4471), whole
//     numbers are parsed ("one hundred and twenty nine" is 129) and a run that
//     is not one number is split at the seam and concatenated ("twenty twenty
//     five" is 2025, "eight forty five" is 845). Adjacent single-digit tokens
//     are joined, "3:30" and "3,520" lose their separator, ordinals lose their
//     suffix ("5th", "fifth" are 5) and "okay" is "ok";
//   - zh is scored per character (CER), en per word (WER).
//
// What it does not model: currency symbols ("$129" against "129 dollars"),
// 块 against 元, "p.m." against "pm", and a name rendered in the other script
// ("Wang Xiaoming" against 王小明). Those count as errors in every
// configuration alike, so they do not move the comparison.

func normZH(s string) string {
	s = strings.ToLower(s)
	var rs []rune
	for _, r := range s {
		if unicode.IsSpace(r) {
			continue
		}
		rs = append(rs, r)
	}
	digit := func(r rune) (int, bool) {
		switch {
		case r >= '0' && r <= '9':
			return int(r - '0'), true
		case r == '〇' || r == '零':
			return 0, true
		}
		for i, c := range []rune("一二三四五六七八九") {
			if r == c {
				return i + 1, true
			}
		}
		if r == '两' {
			return 2, true
		}
		return 0, false
	}
	unit := func(r rune) (int, bool) {
		switch r {
		case '十':
			return 10, true
		case '百':
			return 100, true
		case '千':
			return 1000, true
		case '万':
			return 10000, true
		case '亿':
			return 100000000, true
		}
		return 0, false
	}
	var out strings.Builder
	for i := 0; i < len(rs); {
		_, isD := digit(rs[i])
		_, isU := unit(rs[i])
		if !isD && !isU {
			out.WriteRune(rs[i])
			i++
			continue
		}
		j := i
		hasUnit := false
		for j < len(rs) {
			if _, ok := digit(rs[j]); ok {
				j++
			} else if _, ok := unit(rs[j]); ok {
				hasUnit = true
				j++
			} else {
				break
			}
		}
		run := rs[i:j]
		i = j
		if !hasUnit {
			for _, r := range run {
				d, _ := digit(r)
				out.WriteString(strconv.Itoa(d))
			}
			continue
		}
		var total, section, num int
		for _, r := range run {
			if d, ok := digit(r); ok {
				num = num*10 + d
				continue
			}
			u, _ := unit(r)
			switch {
			case u >= 10000:
				total = (total + section + num) * u
				section, num = 0, 0
			default:
				if num == 0 {
					num = 1 // 十九 is 19
				}
				section += num * u
				num = 0
			}
		}
		out.WriteString(strconv.Itoa(total + section + num))
	}
	var res []rune
	for _, r := range out.String() {
		if unicode.IsLetter(r) || unicode.IsNumber(r) {
			res = append(res, r)
		}
	}
	return string(res)
}

var enUnits = map[string]int{"one": 1, "two": 2, "three": 3, "four": 4, "five": 5, "six": 6, "seven": 7, "eight": 8, "nine": 9}
var enTeens = map[string]int{"ten": 10, "eleven": 11, "twelve": 12, "thirteen": 13, "fourteen": 14, "fifteen": 15,
	"sixteen": 16, "seventeen": 17, "eighteen": 18, "nineteen": 19}
var enTens = map[string]int{"twenty": 20, "thirty": 30, "forty": 40, "fifty": 50, "sixty": 60, "seventy": 70, "eighty": 80, "ninety": 90}
var enOrdinals = map[string]string{"first": "1", "second": "2", "third": "3", "fourth": "4", "fifth": "5", "sixth": "6",
	"seventh": "7", "eighth": "8", "ninth": "9", "tenth": "10", "eleventh": "11", "twelfth": "12", "thirteenth": "13",
	"fourteenth": "14", "fifteenth": "15", "sixteenth": "16", "seventeenth": "17", "eighteenth": "18", "nineteenth": "19",
	"twentieth": "20", "twenty-first": "21", "twenty-second": "22", "twenty-third": "23", "thirtieth": "30"}

func isNumWord(w string) bool {
	if w == "zero" || w == "hundred" || w == "thousand" {
		return true
	}
	_, a := enUnits[w]
	_, b := enTeens[w]
	_, c := enTens[w]
	return a || b || c
}

func normEN(s string) string {
	s = strings.ToLower(s)
	s = strings.ReplaceAll(s, "wi-fi", "wifi")
	// "3:30" and "3,520" lose the separator; every other mark is a space.
	var b strings.Builder
	rs := []rune(s)
	for i, r := range rs {
		switch {
		case r == '\'' || r == '’':
		case (r == ':' || r == ',') && i > 0 && i+1 < len(rs) && unicode.IsDigit(rs[i-1]) && unicode.IsDigit(rs[i+1]):
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			b.WriteRune(r)
		default:
			b.WriteRune(' ')
		}
	}
	toks := strings.Fields(b.String())
	var out []string
	for i := 0; i < len(toks); {
		t := toks[i]
		if strings.HasSuffix(t, "th") || strings.HasSuffix(t, "st") || strings.HasSuffix(t, "nd") || strings.HasSuffix(t, "rd") {
			if _, err := strconv.Atoi(t[:len(t)-2]); err == nil {
				out = append(out, t[:len(t)-2])
				i++
				continue
			}
		}
		if o, ok := enOrdinals[t]; ok {
			out = append(out, o)
			i++
			continue
		}
		if t == "okay" {
			t = "ok"
		}
		if !isNumWord(t) {
			out = append(out, t)
			i++
			continue
		}
		// A run of number words, with "and" tolerated after hundred/thousand.
		var groups []string
		var total, sect int
		var last string // "", unit, zero, teen, tens, hundred, thousand
		flush := func() {
			if last != "" {
				groups = append(groups, strconv.Itoa(total+sect))
			}
			total, sect, last = 0, 0, ""
		}
		for i < len(toks) {
			w := toks[i]
			if w == "and" && (last == "hundred" || last == "thousand") && i+1 < len(toks) && isNumWord(toks[i+1]) {
				i++
				continue
			}
			if !isNumWord(w) {
				break
			}
			i++
			switch {
			case w == "zero":
				flush()
				last = "zero"
			case w == "hundred":
				if last == "unit" && sect > 0 && sect < 10 {
					sect *= 100
				} else {
					flush()
					sect = 100
				}
				last = "hundred"
			case w == "thousand":
				if last == "" {
					total = 1000
				} else {
					total += sect * 1000
					sect = 0
				}
				last = "thousand"
			default:
				if v, ok := enUnits[w]; ok {
					if last != "tens" && last != "hundred" && last != "thousand" && last != "" {
						flush()
					}
					sect += v
					last = "unit"
				} else if v, ok := enTeens[w]; ok {
					if last != "hundred" && last != "thousand" {
						flush()
					}
					sect += v
					last = "teen"
				} else {
					v := enTens[w]
					if last != "hundred" && last != "thousand" {
						flush()
					}
					sect += v
					last = "tens"
				}
			}
		}
		flush()
		out = append(out, strings.Join(groups, ""))
	}
	// Join adjacent single-digit tokens: "4 4 7 1" is 4471.
	var joined []string
	inRun := false
	for _, t := range out {
		single := len(t) == 1 && t[0] >= '0' && t[0] <= '9'
		if single && inRun {
			joined[len(joined)-1] += t
			continue
		}
		joined = append(joined, t)
		inRun = single
	}
	return strings.Join(joined, " ")
}

// editDistance is Levenshtein over any comparable sequence.
func editDistance[T comparable](a, b []T) int {
	prev := make([]int, len(b)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(a); i++ {
		cur := make([]int, len(b)+1)
		cur[0] = i
		for j := 1; j <= len(b); j++ {
			c := 1
			if a[i-1] == b[j-1] {
				c = 0
			}
			cur[j] = min(prev[j]+1, cur[j-1]+1, prev[j-1]+c)
		}
		prev = cur
	}
	return prev[len(b)]
}

// errorRate is CER for zh and WER for en, hypothesis against reference.
func errorRate(lang, ref, hyp string) float64 {
	if lang == "zh" {
		r, h := []rune(normZH(ref)), []rune(normZH(hyp))
		if len(r) == 0 {
			return 0
		}
		return float64(editDistance(r, h)) / float64(len(r))
	}
	r, h := strings.Fields(normEN(ref)), strings.Fields(normEN(hyp))
	if len(r) == 0 {
		return 0
	}
	return float64(editDistance(r, h)) / float64(len(r))
}

// wrongLanguage applies the rule of issue #127: a zh text with no Han
// character, or an en text with any Han character, is in the wrong language.
func wrongLanguage(lang, text string) bool {
	han := false
	for _, r := range text {
		if unicode.Is(unicode.Han, r) {
			han = true
			break
		}
	}
	if lang == "zh" {
		return !han
	}
	return han
}

func TestLangDriftNormalisation(t *testing.T) {
	t.Parallel()
	zh := []struct{ a, b string }{
		{"订单号是4 4 7 1 8 2 9。", "订单号是四四七一八二九"},
		{"一共是3520块", "一共是三千五百二十块"},
		{"套餐费用是129块9毛", "套餐费用是一百二十九块九毛"},
		{"您的欠费金额是2080元", "您的欠费金额是两千零八十元"},
		{"退款12500块", "退款一万二千五百块"},
		{"2025年12月19号", "二零二五年十二月十九号"},
		{"下周3下午3点", "下周三下午三点"},
		{"Wi-Fi信号", "WiFi信号"},
		{"升级到20G", "升级到二十G"},
	}
	for _, c := range zh {
		if a, b := normZH(c.a), normZH(c.b); a != b {
			t.Errorf("zh %q -> %q, %q -> %q", c.a, a, c.b, b)
		}
	}
	en := []struct{ a, b string }{
		{"order number is 4471829.", "order number is four four seven one eight two nine."},
		{"order number is 4471829", "order number is 4 4 7 1 8 2 9"},
		{"3520 dollars", "three thousand five hundred and twenty dollars"},
		{"129 dollars", "One hundred and twenty-nine dollars"},
		{"2080 dollars", "two thousand and eighty dollars"},
		{"12500 dollar", "twelve thousand five hundred dollar"},
		{"December 19, 2025", "December nineteenth, twenty twenty five"},
		{"December 19, 2025", "December 19th, two thousand twenty five"},
		{"at 1030 in", "at 10:30 in"},
		{"at 845", "at eight forty five"},
		{"March 5 next", "March fifth next"},
		{"the 15 of", "the fifteenth of"},
		{"345 minutes", "three hundred and forty five minutes"},
		{"35 employees", "thirty-five employees"},
		{"Wi-Fi works", "wifi works"},
		{"OK", "Okay."},
		{"month's bill", "months bill"},
	}
	for _, c := range en {
		if a, b := normEN(c.a), normEN(c.b); a != b {
			t.Errorf("en %q -> %q, %q -> %q", c.a, a, c.b, b)
		}
	}
	if errorRate("en", "Hello.", "halo.") != 1 {
		t.Error("halo is a full word error against Hello")
	}
	if got := errorRate("zh", "订单号是4471829", "订单号是四四七一八二九"); got != 0 {
		t.Errorf("zh CER = %v", got)
	}
	if !wrongLanguage("zh", "Nineteen twenty") || wrongLanguage("zh", "对。") ||
		!wrongLanguage("en", "翻译") || wrongLanguage("en", "Thank you") {
		t.Error("wrongLanguage rule")
	}
}
