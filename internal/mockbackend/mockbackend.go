// SPDX-License-Identifier: Apache-2.0

// Package mockbackend serves the business APIs the reference flows call,
// so a bot can be walked end to end without a real CRM behind it.
//
// The five flows ported from the reference set (internal/seed/flows) all reach
// a backend for their facts — a repair order, an overdue bill, an appointment
// slot. Without one every tool fails, and a failing tool is the *correct*
// behaviour for a bot that must not invent facts, which makes it useless for
// checking whether the bot can hold a conversation. This supplies the facts.
//
// Fixtures are deliberately the reference ones, so a walkthrough has a
// determinate right answer: RMA1001 is in repair, 92223333 is 8 days overdue,
// 18688886666 (the dev trunk's external caller) is 16 days overdue, 10086002
// is inside its contract and must go to an agent. Somebody reading the
// walkthrough can tell "the bot said the wrong thing" from "the bot said what
// the data says".
//
// cmd/aicc-mockbackend wraps it in a listener; the flow simulator
// (internal/flowsim) serves it in-process.
//
//	go run ./cmd/aicc-mockbackend -addr 127.0.0.1:8770
//	AICC_BOT_BACKEND_BASE=http://127.0.0.1:8770 /tmp/aicc
//
// State lives in memory and only where a flow needs to observe its own writes
// (an appointment booked, then rescheduled, then confirmed). Restarting resets
// it, which is what a walkthrough wants: every run starts from the same facts.
package mockbackend

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// The envelope every response carries. retCode "000000" is what the flows'
// successWhen tests against; anything else is a business failure the bot is
// expected to speak about rather than crash on.
const (
	codeOK   = "000000"
	codeFail = "500000"
)

type store struct {
	mu sync.Mutex
	// appointments by phone. One is pre-seeded so the "manage an existing
	// booking" branch has something to find on a fresh start.
	appointments map[string]*appointment
	apptSeq      int
	orderSeq     int
	// tickets maps a plan-change ticket to how many times it was checked.
	tickets map[string]int
	// promises and leads are written and never read: the flows only need the
	// write to succeed. They are kept so a walkthrough can see arrivals in the
	// log rather than take the response on trust.
	promises int
	leads    int
	logf     func(format string, args ...any)
}

type appointment struct {
	ID        string
	Item      string
	Slot      string
	Address   string
	Status    string
	Confirmed bool
}

func newStore() *store {
	return &store{
		appointments: map[string]*appointment{
			"91234567": {
				ID:      "AP1999",
				Item:    "星讯 X1 手机屏幕维修",
				Slot:    "明天上午 9点到12点",
				Address: "湖畔花园 3 栋 502",
				Status:  "booked",
			},
		},
		apptSeq:  2000,
		orderSeq: 12000,
		tickets:  map[string]int{},
	}
}

// New returns the mock backend. logf receives one line per exchange; nil
// discards them.
func New(logf func(format string, args ...any)) http.Handler {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	s := newStore()
	s.logf = logf
	mux := http.NewServeMux()
	for path, handler := range map[string]func(map[string]any) (any, string){
		"/api/repair/status":   s.repairStatus,
		"/api/appt/get":        s.apptGet,
		"/api/appt/slots":      s.apptSlots,
		"/api/appt/book":       s.apptBook,
		"/api/appt/reschedule": s.apptReschedule,
		"/api/appt/confirm":    s.apptConfirm,
		"/api/plan/check":      s.planCheck,
		"/api/plan/verify":     s.planVerify,
		"/api/plan/submit":     s.planSubmit,
		"/api/plan/status":     s.planStatus,
		"/api/kb/search":       s.kbSearch,
		"/api/bill/overdue":    s.billOverdue,
		"/api/bill/promise":    s.billPromise,
		"/api/bill/dispute":    s.billDispute,
		"/api/lead/submit":     s.leadSubmit,
	} {
		mux.HandleFunc(path, serve(path, s.logf, handler))
	}
	return mux
}

// serve reads the request body, logs the exchange and writes the envelope.
//
// Every call is logged with its arguments, because half of what a walkthrough
// needs to know is whether the bot called the tool at all — and with what. A
// bot that asked for the wrong repair order and a bot that never asked look
// identical from the caller's side of the phone.
func serve(path string, logf func(string, ...any), handler func(map[string]any) (any, string)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		body := map[string]any{}
		if r.Body != nil {
			_ = json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&body)
		}
		data, failure := handler(body)

		out := map[string]any{"retCode": codeOK, "retMsg": "Success"}
		if failure != "" {
			out = map[string]any{"retCode": codeFail, "retMsg": failure}
			logf("%s %v → %s %s", path, body, codeFail, failure)
		} else {
			out["data"] = data
			logf("%s %v → %v", path, body, data)
		}

		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		_ = json.NewEncoder(w).Encode(out)
	}
}

func str(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case float64:
		return strconv.FormatFloat(t, 'f', -1, 64)
	case nil:
		return ""
	default:
		return fmt.Sprint(t)
	}
}

func toInt(v any) int {
	n, _ := strconv.Atoi(strings.TrimSpace(str(v)))
	return n
}

// ------------------------------------------ Handset after-sales: repair status

func (s *store) repairStatus(b map[string]any) (any, string) {
	switch strings.ToUpper(str(b["rmaNo"])) {
	case "RMA1001":
		return map[string]any{"found": "1", "device": "星讯 X1 手机",
			"status": "维修中，已更换屏幕总成，正在整机测试", "eta": "预计2个工作日内寄出"}, ""
	case "RMA1002":
		return map[string]any{"found": "1", "device": "星讯 Air 手机",
			"status": "已维修完成并寄出", "eta": "快递单号尾号 6688，预计明天送达"}, ""
	}
	// found "0" rather than a failure: "no such repair order" is a fact the
	// bot should speak, not an error it should apologise for.
	return map[string]any{"found": "0", "device": "", "status": "", "eta": ""}, ""
}

// ------------------- Field service: book, reschedule, confirm an appointment

// slotText renders a bookable window in the words a bot can read aloud.
func slotText(choice int) string {
	day := time.Now().AddDate(0, 0, 1)
	if choice == 2 {
		day = time.Now().AddDate(0, 0, 2)
		return fmt.Sprintf("%d月%d日下午 2点到5点", day.Month(), day.Day())
	}
	return fmt.Sprintf("%d月%d日上午 9点到12点", day.Month(), day.Day())
}

func (s *store) apptGet(b map[string]any) (any, string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	appt := s.appointments[str(b["phone"])]
	if appt == nil || appt.Status != "booked" {
		return map[string]any{"found": "0"}, ""
	}
	return map[string]any{"found": "1", "appt_id": appt.ID, "item": appt.Item,
		"slot": appt.Slot, "address": appt.Address}, ""
}

func (s *store) apptSlots(map[string]any) (any, string) {
	return map[string]any{"slot1": slotText(1), "slot2": slotText(2)}, ""
}

func (s *store) apptBook(b map[string]any) (any, string) {
	phone, choice := str(b["phone"]), toInt(b["date_choice"])
	if phone == "" || (choice != 1 && choice != 2) {
		return nil, "phone/date_choice required"
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.apptSeq++
	appt := &appointment{
		ID: fmt.Sprintf("AP%d", s.apptSeq), Item: str(b["item"]),
		Slot: slotText(choice), Address: str(b["address"]), Status: "booked",
	}
	s.appointments[phone] = appt
	return map[string]any{"appt_id": appt.ID, "slot": appt.Slot}, ""
}

func (s *store) apptReschedule(b map[string]any) (any, string) {
	choice := toInt(b["date_choice"])
	s.mu.Lock()
	defer s.mu.Unlock()
	appt := s.appointments[str(b["phone"])]
	if appt == nil || (choice != 1 && choice != 2) {
		return nil, "no appointment or bad date_choice"
	}
	appt.Slot = slotText(choice)
	return map[string]any{"appt_id": appt.ID, "slot": appt.Slot}, ""
}

func (s *store) apptConfirm(b map[string]any) (any, string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	appt := s.appointments[str(b["phone"])]
	if appt == nil {
		return nil, "no appointment"
	}
	appt.Confirmed = true
	return map[string]any{"confirmed": "1", "slot": appt.Slot}, ""
}

// ------------------------------------------------ Broadband plan changes
//
// The plan_change flow's backend. plan_check is the decision tool of
// docs/flow-authoring.md section 4.1: the business rule lives here, and the
// flow only branches on the decision. Every text field is answered in the
// request's lang ("zh" or, for anything else, "en").
//
// Service numbers:
//
//	10086001  OK_TO_CHANGE    phone ends 3333; plan 3 is refused at submit
//	10086002  NEED_HUMAN      phone ends 7777; in contract (early-termination fee)
//	10086003  NEED_ID_VERIFY  phone ends 5555; verify, then plan 1-3 may change
//	10086004  NEED_HUMAN      phone ends 9999; account in arrears
//	other     NOT_FOUND

type planAccount struct {
	decision, reason string
	currentPlan      [2]string // en, zh
	phoneLast4       string
	// refusedPlan is a plan id plan_submit refuses for this account (0: none).
	refusedPlan int
}

var planAccounts = map[string]planAccount{
	"10086001": {decision: "OK_TO_CHANGE", reason: "ELIGIBLE", phoneLast4: "3333", refusedPlan: 3,
		currentPlan: [2]string{"100M fibre, 68 yuan a month", "100M 光纤，每月 68 元"}},
	"10086002": {decision: "NEED_HUMAN", reason: "IN_CONTRACT_EARLY_TERMINATION_FEE", phoneLast4: "7777",
		currentPlan: [2]string{"300M fibre, 98 yuan a month, under contract", "300M 光纤，每月 98 元（合约期内）"}},
	"10086003": {decision: "NEED_ID_VERIFY", reason: "CHANGE_REQUIRES_ID_CHECK", phoneLast4: "5555",
		currentPlan: [2]string{"200M fibre, 78 yuan a month", "200M 光纤，每月 78 元"}},
	"10086004": {decision: "NEED_HUMAN", reason: "ARREARS", phoneLast4: "9999",
		currentPlan: [2]string{"100M fibre, 68 yuan a month, 2 months overdue", "100M 光纤，每月 68 元（已欠费2个月）"}},
}

// planText picks the wording for the request's language.
func planText(lang, en, zh string) string {
	if strings.EqualFold(strings.TrimSpace(lang), "zh") {
		return zh
	}
	return en
}

func planOptions(lang string) string {
	return planText(lang,
		"Plan 1, 300M fibre, 88 yuan a month. Plan 2, 500M fibre, 108 yuan a month. "+
			"Plan 3, gigabit fibre, 158 yuan a month.",
		"套餐一，300M 光纤，每月 88 元；套餐二，500M 光纤，每月 108 元；套餐三，千兆光纤，每月 158 元。")
}

// planCheck answers with a decision on a success envelope: "the backend
// decided no" is a field, not a failure, so the flow can read it.
func (s *store) planCheck(b map[string]any) (any, string) {
	lang := str(b["lang"])
	acct, ok := planAccounts[str(b["serviceNo"])]
	if !ok {
		return map[string]any{"decision": "NOT_FOUND", "reason": "UNKNOWN_SERVICE_NUMBER",
			"spoken":  planText(lang, "That service number was not found.", "没有查到这个业务号码。"),
			"options": "", "current_plan": ""}, ""
	}
	out := map[string]any{"decision": acct.decision, "reason": acct.reason,
		"current_plan": planText(lang, acct.currentPlan[0], acct.currentPlan[1]), "options": ""}
	switch acct.decision {
	case "OK_TO_CHANGE":
		out["spoken"] = planText(lang, "This account can change plan now.", "该账户可以直接变更套餐。")
		out["options"] = planOptions(lang)
	case "NEED_ID_VERIFY":
		out["spoken"] = planText(lang,
			"This change needs a quick identity check first, using the registered phone number.",
			"办理这项变更前需要先核验身份，核对登记的电话号码。")
		out["options"] = planOptions(lang)
	default:
		if acct.reason == "ARREARS" {
			out["spoken"] = planText(lang, "The account has an overdue balance, so an agent must handle it.",
				"账户有欠费，需要人工客服处理。")
		} else {
			out["spoken"] = planText(lang,
				"The account is under contract and an early-termination fee applies, so an agent must handle it.",
				"账户在合约期内，变更会产生违约金，需要人工客服处理。")
		}
	}
	return out, ""
}

func (s *store) planVerify(b map[string]any) (any, string) {
	acct, ok := planAccounts[str(b["serviceNo"])]
	match := "0"
	if ok && acct.phoneLast4 == str(b["phoneLast4"]) {
		match = "1"
	}
	return map[string]any{"match": match}, ""
}

// planSubmit returns a ticket at once, as a slow operation must (flow-authoring
// section 4.3). The work "continues in the background": planStatus reports it.
func (s *store) planSubmit(b map[string]any) (any, string) {
	lang := str(b["lang"])
	no, plan := str(b["serviceNo"]), toInt(b["planId"])
	acct, ok := planAccounts[no]
	if !ok || plan < 1 || plan > 3 {
		return nil, "serviceNo/planId required"
	}
	if acct.decision == "NEED_HUMAN" {
		return nil, "this account cannot change plan by self-service"
	}
	if plan == acct.refusedPlan {
		return nil, "this plan is not available at the installation address"
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.orderSeq++
	ticket := fmt.Sprintf("PC-%d", s.orderSeq)
	s.tickets[ticket] = 0
	return map[string]any{"ticket": ticket, "state": "PROCESSING",
		"effective": planText(lang, "effective from the next billing cycle", "下一个账期生效")}, ""
}

// planStatus is PROCESSING on the first check of a ticket and DONE on every
// later one, so a walkthrough sees both without waiting.
func (s *store) planStatus(b map[string]any) (any, string) {
	lang := str(b["lang"])
	ticket := str(b["ticket"])
	s.mu.Lock()
	defer s.mu.Unlock()
	checks, ok := s.tickets[ticket]
	if !ok {
		return nil, "ticket not found"
	}
	s.tickets[ticket] = checks + 1
	if checks == 0 {
		return map[string]any{"state": "PROCESSING",
			"eta": planText(lang, "about 10 minutes", "大约10分钟")}, ""
	}
	return map[string]any{"state": "DONE", "eta": ""}, ""
}

// ----------------------------------------------------------- Knowledge base

type kbEntry struct {
	keywords []string
	en, zh   string
}

// The toy retrieval service behind kb_search. A real one lives outside this
// repository (flow-authoring section 4.2); this one matches keywords, which is
// enough to walk the found and not-found branches. The first entry with a
// matching keyword wins, so order is specificity.
var kbEntries = []kbEntry{
	{[]string{"early termination", "termination fee", "违约金", "提前解约", "解约"},
		"Changing plan inside the contract term costs an early-termination fee of 200 yuan, so an agent handles it.",
		"合约期内变更套餐需支付200元违约金，由人工客服办理。"},
	{[]string{"installation", "install", "安装费", "安装", "装机"},
		"The installation fee is 100 yuan for a new line. A plan change on an existing line costs nothing.",
		"新装宽带安装费100元；已有线路变更套餐不收费。"},
	{[]string{"take effect", "effective", "when will", "生效", "什么时候"},
		"A plan change takes effect from the next billing cycle.",
		"套餐变更从下一个账期开始生效。"},
	{[]string{"contract", "term", "合约", "合同", "期限"},
		"Contracts run 12 months. A plan change outside a contract carries no fee.",
		"合约期为12个月；合约期外变更套餐不收费。"},
	{[]string{"gigabit", "1000m", "千兆"},
		"Gigabit fibre is 158 yuan a month, up to 1000M download.",
		"千兆光纤每月158元，下行最高1000M。"},
	{[]string{"500m", "plan 2", "套餐二"},
		"The 500M plan is 108 yuan a month, up to 500M download.",
		"500M 套餐每月108元，下行最高500M。"},
	{[]string{"300m", "plan 1", "套餐一"},
		"The 300M plan is 88 yuan a month, up to 300M download.",
		"300M 套餐每月88元，下行最高300M。"},
	{[]string{"price", "cost", "how much", "speed", "价格", "多少钱", "资费", "网速", "速度"},
		"Plans: 300M at 88 yuan, 500M at 108 yuan and gigabit at 158 yuan a month.",
		"套餐：300M 每月88元，500M 每月108元，千兆每月158元。"},
}

func (s *store) kbSearch(b map[string]any) (any, string) {
	query := strings.ToLower(str(b["query"]))
	lang := str(b["lang"])
	for _, e := range kbEntries {
		for _, k := range e.keywords {
			if strings.Contains(query, k) {
				return map[string]any{"found": "1", "answer": planText(lang, e.en, e.zh)}, ""
			}
		}
	}
	return map[string]any{"found": "0", "answer": ""}, ""
}

// ------------------------------------- Bill reminders (early collections)

func (s *store) billOverdue(b map[string]any) (any, string) {
	switch str(b["phone"]) {
	case "92223333":
		return map[string]any{"found": "1", "amount": "45.60",
			"due_date": "上月28日", "days_overdue": "8"}, ""
	case "90000000":
		return map[string]any{"found": "1", "amount": "196.00",
			"due_date": "上月15日", "days_overdue": "21"}, ""
	case "18688886666":
		return map[string]any{"found": "1", "amount": "128.50",
			"due_date": "上月20日", "days_overdue": "16"}, ""
	}
	return map[string]any{"found": "0", "amount": "", "due_date": "", "days_overdue": ""}, ""
}

func (s *store) billPromise(b map[string]any) (any, string) {
	if str(b["payDate"]) == "" {
		return nil, "payDate required"
	}
	s.mu.Lock()
	s.promises++
	n := s.promises
	s.mu.Unlock()
	s.logf("  付款安排已记录，累计 %d 条", n)
	return map[string]any{"recorded": "1"}, ""
}

func (s *store) billDispute(map[string]any) (any, string) {
	s.mu.Lock()
	s.promises++
	s.mu.Unlock()
	return map[string]any{"recorded": "1"}, ""
}

// ------------------------------------------------------------- Sales leads

func (s *store) leadSubmit(b map[string]any) (any, string) {
	grade := map[string]string{"high": "A", "mid": "B", "low": "C"}[str(b["interest"])]
	if grade == "" {
		grade = "D"
	}
	s.mu.Lock()
	s.leads++
	n := s.leads
	s.mu.Unlock()
	s.logf("  线索已记录，评级 %s，累计 %d 条 (interest=%q budget=%q timeline=%q)",
		grade, n, str(b["interest"]), str(b["budget"]), str(b["timeline"]))
	return map[string]any{"lead_grade": grade, "recorded": "1"}, ""
}
