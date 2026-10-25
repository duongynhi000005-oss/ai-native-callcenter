// SPDX-License-Identifier: Apache-2.0

package wrapupdraft

import (
	"strings"
	"testing"
)

func TestNoteLanguage(t *testing.T) {
	cases := []struct {
		name  string
		lines []string
		want  string
	}{
		{"pure English", []string{"I need my invoice resent", "Done, it is in your inbox"}, "en"},
		{"pure Chinese", []string{"我想查一下账单", "好的，已经发到您的邮箱了"}, "zh"},
		{"Chinese-led with English terms", []string{"我想把 shipping address 改成公司新地址", "好的，订单上的 address 已更新", "OK 谢谢"}, "zh"},
		{"English-led with Chinese words", []string{"你好, I need to change my 送货地址 to the new office please", "Sure, what is the new office address?", "是 Building 3, 88 Park Road, Shanghai, 邮编 200120"}, "en"},
		{"digits and punctuation only", []string{"555 0142 ...", "?!"}, "en"},
		{"empty", nil, "en"},
	}
	for _, c := range cases {
		var ls []line
		for _, x := range c.lines {
			ls = append(ls, line{speaker: "Customer", text: x})
		}
		if got := noteLanguage(ls); got != c.want {
			t.Errorf("%s: got %q want %q", c.name, got, c.want)
		}
	}
}

func TestRenderStatesTheNoteLanguageLast(t *testing.T) {
	p, ok := render(segments{agent: []line{{"Customer", "我想查账单"}}}, seededDispositions, DefaultBudgetChars)
	if !ok || !strings.HasSuffix(p, "Write the note in Chinese (简体中文). Call the agent 坐席 and the customer 客户.\n") {
		t.Fatalf("ok=%v prompt=%q", ok, p)
	}
	p, _ = render(segments{agent: []line{{"Customer", "my bill"}}}, seededDispositions, DefaultBudgetChars)
	if !strings.HasSuffix(p, "Write the note in English.\n") {
		t.Fatalf("prompt=%q", p)
	}
}
