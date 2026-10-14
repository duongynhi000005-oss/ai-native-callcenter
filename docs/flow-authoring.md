# Authoring flows

A guide for writing a flow that stays small and readable when the business behind it is not. It covers what the flow DSL does, where each kind of logic belongs, and what the DSL cannot do yet.

Every field, built-in and behaviour named here exists in the code today. Where something a business may want is not supported, the guide says so and links the issue. It does not describe designs that are not built.

Related reading: the field reference is `internal/flow/spec.go`; the engine design and the live findings that amended it are `docs/design/02-ai-voice.md` §6 and §7; the seeded flows in `internal/seed/flows/` are working examples; the mock backend they call is `cmd/aicc-mockbackend`.

## 1. What a flow is, and what it is not

A flow is one JSON document: a persona, a set of **phases** (`nodes`), the **tools** the model may call in each phase, and **transitions** that move the call from one phase to another.

The division of labour is the whole design:

- **The model owns the conversation.** What to say, how to ask again, how to phrase a number.
- **The flow owns the phase.** Which instruction the model is under now, which tools it may call, and when that changes. The engine never writes dialogue.
- **The backend owns the business rules and the facts.** Eligibility, prices, availability, policy text.

The flow is configuration, not a program. Conditions are a closed set of operators over slots, with no expressions and no derived values (`internal/flow/predicate.go`). That is deliberate, and it is the reason the patterns in section 4 exist: when you need a decision, put it in a service and let the flow branch on the answer.

A flow that has grown past a handful of phases usually has one of three symptoms, and each has a cure in section 4:

| Symptom | Cure |
| --- | --- |
| Chains of phases and `condition` trees spelling out eligibility, pricing or multi-step validation | Thick tools, thin flow (4.1) |
| Policies, tariffs or FAQ text pasted into `persona`, `rules` or an `instruction` | Knowledge through a tool (4.2) |
| An operation that is slow, or that the bot should not complete alone, modelled as an ordinary tool | Long operations (4.3), risky operations (4.4) |

## 2. Anatomy of a flow

The loader (`flow.Load`, `internal/flow/load.go`) rejects a flow that cannot run, and reports every problem at once. Dialect version is `"specVersion": "v2"`.

### 2.1 Top level

| Field | Meaning |
| --- | --- |
| `id` | Required. The flow's slug; `aicc flowadd` updates an existing flow by it. |
| `specVersion` | Required, must be `"v2"`. |
| `entry` | Optional name of the business entry point. Seeded into the `entry` slot. |
| `initialNode` | Required. A call starts in this phase. |
| `global` | Persona, rules and guards that apply across every phase (2.2). |
| `nodes` | Map of phase id to phase (2.3). At least one. |
| `tools` | Map of tool name to a declarative HTTP tool (2.4). |

### 2.2 `global`

| Field | Meaning |
| --- | --- |
| `persona` | Required. The character the model adopts. Part of the standing instructions on every turn. |
| `rules` | Standing rules, one string each (or a per-language list). Rendered as a bullet list after the persona. |
| `voice` | The bot's voice, versioned with the flow. Empty uses the provider's default. |
| `temperature` | Sampling temperature, `0 <= t < 2`. Providers without the setting ignore it. |
| `fallbackTarget` | A phase the engine moves to when `maxTurns` is exceeded. |
| `maxTurns` | Bound on tool dispatches for the whole call, against a model that loops. |
| `closingTarget` | The flow's own goodbye: a **terminal** phase the engine moves to by itself (5.3). |
| `maxTurnsWithoutTool` | Bound on tool-less replies in one phase (5.3). Requires `closingTarget`. |
| `maxDurationSec` | The call's wall clock (5.4). Absent means 900, `0` is off, otherwise 60 to 3600. |
| `alwaysAllowedTools` | Tools allowed in every phase, so a request for a person is never blocked by the phase. |
| `transitions` | Rules that apply in every non-terminal phase, after the phase's own. |
| `apiBaseEnv` | Parsed but has no effect. The backend address comes only from `AICC_BOT_BACKEND_BASE` (`docs/dev-stack.md`). |

### 2.3 A phase

| Field | Meaning |
| --- | --- |
| `instruction` | Required. What the model is trying to accomplish now. A brief the model writes from. May reference `{slots.name}`. |
| `announce` | Optional. A line spoken **as written** on entering the phase, also rendered with `{slots.name}`. |
| `tools` | The allowlist for this phase. `"*"` allows every tool. Empty allows only `alwaysAllowedTools`. |
| `transitions` | Rules that move the call out of this phase (2.5). |
| `isTerminal` | The conversation does not leave this phase. A terminal phase may not declare `transitions`. |

Every user-facing string (`persona`, `instruction`, `announce`, tool `description`) is either a plain string, meaning the same text in both languages, or an object `{"en": "...", "zh": "..."}`. A missing translation falls back to the other language rather than to silence. One flow serves every language; business logic is never duplicated per language, only wording. The call's `language` picks the wording and nothing else; it never selects the provider or the voice.

`{slots.name}` is substituted into `instruction` and `announce` only. It is not substituted into `persona` or `rules`. A slot that is not set renders as nothing.

### 2.4 A tool

A declarative tool is by default an HTTP call to your backend (a second kind, the slot tool, records what the caller said and calls nothing: see 2.8). The model sees `description` and `parameters` (a JSON Schema object, passed to the model unchanged); the engine runs the `http` block.

| Field | Meaning |
| --- | --- |
| `description` | Required. The model reads this to decide when to call the tool. |
| `parameters` | JSON Schema of the arguments. |
| `http.path` | Required. Appended to `AICC_BOT_BACKEND_BASE`. |
| `http.method` | Defaults to `POST`. |
| `http.body` | A template. String values may reference `{args.x}` (the model's arguments) and `{slots.y}` (the call's slots). A value that is exactly one reference keeps its type; a reference inside text becomes text. |
| `http.successWhen` | `{ "path": "retCode", "equals": "000000" }`. Absent means any 2xx counts. |
| `http.errorFrom` | Dotted path to the backend's own error message when `successWhen` fails. |
| `http.result` | Map of slot name to dotted path in the response. **Only these fields are exposed.** |

Runtime behaviour worth designing around (`internal/flow/httptool.go`):

- A call is bounded at **5 seconds**. A timeout, a connection failure, a status of 400 or above, or a body that is not a JSON object is a tool failure: the model is told the operation could not be completed and, per the built-in hint, to offer a person if it fails again.
- A `successWhen` that does not hold is a **refusal**, not a failure: the result is `ok: "0"` with the `errorFrom` message. No `result` field is mapped on a refusal.
- Only the fields in `http.result` ever reach the model or the slots. A whole backend response is never handed over.
- A tool is offered to the model only if some phase's allowlist, or `alwaysAllowedTools`, names it. A tool called outside its phase is answered with `ok: "0"` and the current phase's instruction, not an error.

### 2.5 Transitions and conditions

A transition has `on`, `tool`, `condition`, `target` and `priority`.

- `on` is `TOOL_RESULT` (a tool ran, whatever its outcome) or `NO_INPUT` (the caller said nothing). Empty matches any event. Nothing else is an event: there is no event for what the caller said, and none for a keypress (see 6).
- `tool` narrows a `TOOL_RESULT` rule to one tool. A `NO_INPUT` rule that names a tool is refused at load, because a silence carries no tool.
- `target` must be an existing phase.
- Rules are tried in ascending `priority`, equal priorities in declared order, the phase's own rules before the flow-wide ones. The first whose event and condition both hold wins. If none holds, the call stays in the phase and the model tries again, which reads to the caller as being asked again, not as an error.
- A rule whose `target` is the current phase is an explicit stay.
- A model can ask for several tools in one response. All of them are judged in the phase the response was made in; moves apply one at a time and the last one wins.

A condition is a leaf `{ "slot", "op", "value" }`, or `{ "all": [...] }`, or `{ "any": [...] }`. Omitted means always true. The operators are `EQ NE GT LT GTE LTE IS_NULL IS_NOT_NULL IS_EMPTY IS_NOT_EMPTY IN NOT_IN CONTAINS NOT_CONTAINS`. A mistyped operator is a load error. `EQ` compares numbers by value, so a backend returning `"1"` matches a rule written `1`.

### 2.6 Slots

Slots are the call's structured state. They live in this application, not in the provider's history, and are re-pinned into the model's standing instructions on a phase change, so they survive a provider truncating a long conversation. The names the engine maintains:

| Slot | Value |
| --- | --- |
| `caller`, `did`, `entry`, `lang` | Seeded at the start of the call. |
| `node` | The current phase id. |
| `result.<field>` | The mapped fields of the **last** tool result, including `result.ok` (`"1"` or `"0"`). Replaced wholesale on every tool result. |
| `<tool>.<field>` | The same fields, kept per tool until that tool runs again. |
| `<tool>.calls` | How many times that tool has run. |
| `lastTool`, `turns` | The last tool that ran; the count of tool dispatches. |
| `<node>.visits` | How many times a phase has been entered. |
| `noInput.count` | Consecutive silences. Reset by words, a keypress or a tool result. |
| `turnsWithoutTool` | Tool-less replies in the current phase (5.3). |

A tool result that changes what the current phase's instruction renders from slots re-pins the instruction without a phase change, so an instruction never quotes a superseded value.

A value becomes a slot in four ways: a `http.result` mapping over a backend response; a slot tool recording a fact the caller said (2.8); the arguments of any tool call, stored as `<tool>.args.<k>`; and the business data an outbound call was placed with, seeded as `userData.<k>`.

| Slot | Value |
| --- | --- |
| `<slot>` | What a slot tool (2.8) recorded. |
| `<tool>.args.<k>` | The scalar arguments of the tool's **last** call. Replaced wholesale on every call, so an argument left out this time is not read from the time before. Strings are cut to 256 characters; objects and arrays are not stored. |
| `userData.<k>` | Business data the call was placed with. Read-only: the model cannot write it. Nested objects are flattened with dots (`userData.order.id`); arrays are skipped. |

### 2.7 Built-in tools

Three tools exist in every flow, and a fourth on campaign calls. They are not HTTP calls; they act on the call itself. A phase allows them by name, or `alwaysAllowedTools` does.

| Tool | Arguments | Behaviour |
| --- | --- | --- |
| `transfer_to_agent` | `queue` (an enum of the deployment's queues, narrowed to the number's own queue when it has one), `reason`, `summary` (at most 600 characters, caller's language), `slots` (an object) | Arms a transfer that runs after the caller has heard the model's line. The `summary`, `reason` and `slots` ride the caller's channel to the agent. A closed or unknown queue is a refusal (`QUEUE_CLOSED`, `QUEUE_UNKNOWN`) with a hint to offer a message. |
| `take_message` | `message`, `callbackNumber` (optional) | Records an open callback request, defaulting the number to the caller's. |
| `hangup` | `isFarewellSpoken` | Ends the call once the goodbye has been heard. |
| `record_lead` | `intent` (`HIGH`, `MEDIUM`, `LOW`, `NONE` or `DO_NOT_CALL`), `notes` (optional, at most 1000 characters; longer is cut) | **Campaign calls only.** Records what the call concluded about the customer on the call's campaign attempt; the last call wins, so call it again when the customer changes their mind. It is bound to no phase: it is offered and callable in every phase whatever `tools` and `alwaysAllowedTools` say, it never moves the phase and never counts as a turn, and it is not offered on any other call. An intent outside the enum comes back as a tool error and nothing is written. The call is not ended by it. The side effects run once, when the call ends: `DO_NOT_CALL` puts the number on the do-not-call list, `HIGH` files one callback on the campaign's follow-up queue (design 10 section 11). A campaign call is offered neither `transfer_to_agent` nor `take_message`, so a flow meant for campaigns lists neither (`seed/flows/campaign_lead_followup.json` is the reference). |

Two details that matter when authoring:

- `slots` in `transfer_to_agent` is an argument the **model** fills in. The engine's slots are not attached automatically. If the agent must receive a decision or an id, say so in the phase instruction so the model passes it, or put it in the `summary`.
- `reason` is a free string, not an enum, even though the design text in `docs/design/02-ai-voice.md` §6 shows one.

A built-in's refusal is a conversation, not an error. When a queue is closed the model explains and offers a message. Business hours therefore belong to the queue, not to a branch in the flow.

### 2.8 Slot tools: facts the caller states

A fact the caller merely said ("my budget is 5000", "next month") lives only in the model's conversation history, which a long call can truncate. A slot tool writes it down. It is declared in `tools` with `"kind": "slot"` and calls nothing:

```json
"record_budget": {
  "kind": "slot",
  "slot": "budget",
  "description": {
    "en": "Record the budget the caller gave, in their own words, as soon as they answer. Do not call it if they did not answer.",
    "zh": "记录用户说的预算，用用户自己的说法。用户没回答就不要调用。"
  }
},
"record_interest": {
  "kind": "slot", "slot": "interest", "enum": ["high", "mid", "low", "none"],
  "description": { "en": "...", "zh": "..." }
}
```

| Field | Meaning |
| --- | --- |
| `kind` | `"slot"`. Absent or `"http"` is an HTTP tool; anything else is a load error. |
| `slot` | Required. The slot written: `slots.<slot>`. A single name of letters, digits and underscores, unique among slot tools, and never one the engine owns (`caller`, `did`, `entry`, `lang`, `node`, `turns`, `turnsWithoutTool`, `lastTool`, `result`, `userData`, `noInput`). |
| `enum` | Optional. The only values accepted; passed to the model as the enum of its one argument. |
| `description` | Required, bilingual like any tool. Say when to call it and when not to. |

A slot tool has no `http` and no `parameters`; declaring either is a load error. The model is offered one string argument, `value`, generated from the declaration. List the tool in the allowlist of each phase where the fact may be stated, like any other tool.

How it behaves (`internal/flow/slottool.go`):

- **Reading it back.** `{slots.<slot>}` in an `instruction`, an `announce` or an `http.body` renders the value, and a condition can test it (`IS_EMPTY`, `IS_NOT_EMPTY`, `EQ` and the rest). A tool that stays in the phase but changes what the instruction renders re-pins the instruction (2.6), so the bot can be told what is already answered: `Recorded so far (blank means not answered yet): budget={slots.budget}; ...`. A fact recorded in one phase is still there in every later phase and in an HTTP tool's body.
- **Branching.** A slot call is a tool result: `result.ok` is `"1"` and `<tool>.calls` counts calls, so a `TOOL_RESULT` transition on the tool fires, with or without a condition on the slot.
- **The latest call overwrites.** Recording a correction ("not 5000, 8000") replaces the earlier value.
- **Refusals.** An empty or whitespace-only value, a value over **256 characters**, and a value outside the `enum` are answered with `ok: "0"` and the reason. Nothing is stored, no slot changes, and a `TOOL_RESULT` rule that requires `result.ok` of `"1"` does not fire. Over-long is rejected, not truncated: half a fact is never recorded. The model reads the reason and can retry.
- **A declined question stays blank.** If the caller will not answer, the model should not call the tool (say so in its `description`), and the slot stays unset. Write the instruction so a blank is a state: do not ask again, submit what you have. In an `http.body`, a value that is nothing but `{slots.<slot>}` arrives as JSON `null` when the slot is unset (a lone reference keeps the value's type, and there is none); a reference inside other text renders as an empty string. The backend should read `null` as "not answered".
- **A late answer is lost.** A slot tool is an ordinary tool, so it is refused (`ok: "0"`, the current phase's instruction) in any phase whose allowlist does not name it. Once the flow has moved on, for example from the question phase to `done` after the submit, a late answer or correction from the caller is refused and lost. A flow that must accept corrections keeps the slot tools allowed in the later phase and submits again; the seed `lead_qualification` flow does not do that.
- **Models record what you do not mean.** On qwen a model has recorded "it depends" as a budget, invented a value from a noisy transcript, re-asked a declined question, and offered example amounts. Say in the `description` and the instruction what counts as an answer (a concrete amount or range, a concrete time), that "don't know" is a decline, that only the caller's own words are recorded, and that a declined question is not asked again. `lead_qualification` is the reference wording.
- **Normalisation.** Surrounding space is trimmed and line breaks become spaces, so a value cannot add lines to the instruction it is rendered into.
- **Values are untrusted caller text.** They are what a person said, transcribed, and they are rendered into the model's instructions and sent to your backend. Treat them as input: validate them in the backend, and do not let a rule's meaning depend on a slot's content.

Other slots you can read back:

- **`<tool>.args.<k>`** exists for every tool, built-in ones included. It is readable in a template or condition **only for arguments the tool declares** in its `parameters.properties` (a slot tool declares `value`). Referencing anything else is a load error.
- **`userData.<k>`** is accepted for any `k`, because the keys come from whoever placed the call.

**Declared-slot rule, checked at load.** `{slots.x}` in an `instruction`, an `announce` or an `http.body`, and the `slot` of a transition condition, must name something the flow can write, or the flow does not load (a typo would otherwise be heard on a call as an empty gap). The accepted names are: `caller`, `entry`, `did`, `lang`, `node`, `turns`, `turnsWithoutTool`, `lastTool`, `noInput.count`; `userData.<k>`; a slot tool's `slot`; `<node>.visits` for a phase of the flow; `<tool>.calls` and `<tool>.ok`; `<tool>.<field>` where `field` is a key of that HTTP tool's `http.result`; `<tool>.args.<k>` where `k` is a declared parameter; and `result.<field>` for a field some tool maps (`ok` included). `persona` and `rules` are not rendered, so they are not checked.

**Providers.** A slot is engine state, so it is exact in templates and HTTP bodies on every engine. How soon the *model* is re-told differs: the Realtime engines (OpenAI, Qwen, the gateway) re-pin at once; **Doubao and Gemini re-pin best effort**. Gemini cannot change its instructions mid-call, and they reach the model with the next tool result or turn that carries something; Doubao's frame is sent but its effect on a call in progress is not measured. Do not depend on an instant re-pin there: keep the "never ask a recorded question again" rule cheap to obey, and carry a fact that must not be lost in an HTTP body rather than only in the prompt.

## 3. What the platform already does for you

Do not spend flow text on these.

- **Standing platform rules.** Every flow gets two rules ahead of its persona (`Runtime.Instructions`): the model keeps its instructions private, and it must not say it is transferring, looking something up, saving a message or ending the call unless it calls the matching tool in the same reply. Do not repeat them in `rules`.
- **Hint steering.** When a transition fires, the tool result the model receives carries the new phase's instruction as `hint`, which replaces anything the tool would have said. The model reads it as something it just learned.
- **Queue names.** `transfer_to_agent`'s `queue` argument is an enum of real queues, so the model cannot invent one. A phase instruction may still tell it which queue fits (`novanet_support` names `support-en` and `support-zh`).
- **Dead air and loops.** Section 5.

## 4. Patterns for a business that is not simple

### 4.1 Thick tools, thin flow

Put a business rule in the backend. One composite endpoint makes the decision and returns it as a field. `http.result` maps the field to a slot, and the flow branches on it.

Instead of a phase per rule and a `condition` tree over `in_contract`, `tenure` and `balance`, the backend answers:

```json
{ "retCode": "000000", "data": { "decision": "NEED_ID_VERIFY", "reason": "CHANGE_OVER_THRESHOLD", "spoken": "This change needs a quick identity check first." } }
```

and the flow only reads `result.decision`. Changing the rule changes the backend; the published flow does not change and does not need re-publishing.

Guidance:

- **Return the decision on a success envelope.** A backend that answers `NEED_HUMAN` as a failure (`successWhen` not holding) loses every mapped field, because a refusal maps none. Use the failure path for "the backend could not decide", and a field for "the backend decided no".
- **Use a small closed vocabulary** of decision values (`OK_TO_CHANGE`, `NEED_ID_VERIFY`, `NEED_HUMAN`) and one transition per value. A phase with a transition for every value the backend can return, plus a fallback rule with a higher `priority` number, cannot dead-end silently.
- **Let the backend also return what the model should say**, in a short spoken-ready field (`spoken`, `reason`), mapped into the result. The model reads it from the tool result, so the wording of a rule lives next to the rule.
- **Keep the model out of the arithmetic.** The seeded `plan_change` flow tells the model in the `offer` instruction to check `in_contract` and transfer. That works, but it is a business rule held in prompt text, and it is the shape this pattern replaces: a `decision` field and one transition would make it deterministic.

### 4.2 Knowledge through a tool

Policies, tariffs and FAQs do not belong in `persona`, `rules` or an `instruction`. A long standing prompt costs instruction-following and is resent whenever the instructions are re-pinned, which is every phase change.

Give the flow a search tool backed by a retrieval service that lives **outside this repository** (the same boundary as the speech gateway: the extension point is a service behind the flow's backend URL, never code in this tree). Keep the persona and the instructions short, and let a phase's instruction say **when** to search:

```json
{
  "tools": {
    "kb_search": {
      "description": {
        "en": "Search the plan and policy knowledge base. Call it for any question about prices, terms, fees or policy before answering; never answer such questions from memory.",
        "zh": "检索套餐与政策知识库。凡涉及价格、条款、费用或政策的问题，先调用本工具再回答，不要凭记忆作答。"
      },
      "parameters": {
        "type": "object",
        "properties": {
          "query": { "type": "string", "description": "The caller's question, in a few words" }
        },
        "required": ["query"]
      },
      "http": {
        "path": "/api/kb/search",
        "body": { "query": "{args.query}", "lang": "{slots.lang}" },
        "successWhen": { "path": "retCode", "equals": "000000" },
        "errorFrom": "retMsg",
        "result": { "answer": "data.answer", "found": "data.found" }
      }
    }
  }
}
```

Guidance:

- **Return a short, speakable `answer`**, not documents. Whatever the tool maps goes into the model's context for that turn; a page of policy text makes a call slow to answer and hard to steer. Condense in the retrieval service.
- **Return `found` and say what to do when it is empty** in the phase instruction (offer a person or a message), so a miss is a branch and not an invention.
- **Allow the tool in the phases where questions arise** and no wider. A search tool in a phase does not move the call: with no transition for it, the call stays put and the conversation goes on.
- **Do not map large fields into slots on purpose.** Mapped fields are slots, and a slot named in an instruction is re-pinned into the standing instructions.
- A neutral retrieval contract and ready templates for particular stores are a separate piece of work, not built: see 7.

### 4.3 Long operations

An HTTP tool has a fixed 5 second timeout and a caller is waiting on the line. Never write a tool that blocks on a slow job. Two shapes work with what exists:

1. **Submit and check.** One tool starts the work and returns at once with a ticket; a second tool reports its status. The phase instruction says to submit, read out the ticket, and check status when the caller asks. The instruction should say what to tell the caller when the state is still pending. Both calls are ordinary tools, so a pending state is just a field the model reads.

   ```json
   {
     "tools": {
       "plan_submit": {
         "description": "Submit the confirmed plan change. Returns a ticket immediately; processing continues in the background. Only call after the caller explicitly confirms.",
         "parameters": {
           "type": "object",
           "properties": {
             "service_no": { "type": "string" },
             "plan_id": { "type": "string", "enum": ["1", "2", "3"] }
           },
           "required": ["service_no", "plan_id"]
         },
         "http": {
           "path": "/api/plan/submit",
           "body": { "serviceNo": "{args.service_no}", "planId": "{args.plan_id}" },
           "successWhen": { "path": "retCode", "equals": "000000" },
           "errorFrom": "retMsg",
           "result": { "ticket": "data.ticket", "state": "data.state" }
         }
       },
       "plan_status": {
         "description": "Check the progress of a submitted plan change by its ticket.",
         "parameters": {
           "type": "object",
           "properties": { "ticket": { "type": "string" } },
           "required": ["ticket"]
         },
         "http": {
           "path": "/api/plan/status",
           "body": { "ticket": "{args.ticket}" },
           "successWhen": { "path": "retCode", "equals": "000000" },
           "errorFrom": "retMsg",
           "result": { "state": "data.state", "eta": "data.eta" }
         }
       }
     }
   }
   ```

2. **Call back.** When the work cannot finish during the call, tell the caller they will be called back and use `take_message`, which records an open callback an agent works later. Say so in the phase instruction, and offer the same when a tool fails twice.

Do not rely on the caller waiting through silence. `NO_INPUT` fires after the dead-air timeout (`AICC_BOT_NO_INPUT_TIMEOUT`, 8 seconds by default), and `maxDurationSec` (5.4) still runs.

### 4.4 Risky operations

Whether a step needs a person is a business decision, so the backend makes it, not the prompt. Have the decision tool return `NEED_HUMAN` and let a transition send the call to a phase whose only job is to hand over:

```json
{
  "nodes": {
    "need_transfer": {
      "instruction": {
        "en": "This cannot proceed via self-service. Apologise briefly, then immediately call transfer_to_agent. Put the decision and the reason in the summary.",
        "zh": "当前业务无法自助办理：向用户简短道歉，立即调用 transfer_to_agent，并把判定结果和原因写进 summary。"
      },
      "tools": []
    }
  }
}
```

`tools: []` is enough because `transfer_to_agent` is in `alwaysAllowedTools` in the seeded flows. The global transition on a successful `transfer_to_agent` then moves the call to a terminal phase whose `announce` is the hand-over line (the seeded `finish_transfer`). The same phase is the right `fallbackTarget`: a model that loops, or a tool that fails repeatedly, ends in a person. The seeded flows do this by counting calls (`bb_verify.calls GTE 2`, priority 1).

Do not let a prompt rule such as "never change a plan over 200" be the only guard. The model can be talked past a prompt; it cannot call a tool the phase does not allow, and it cannot make the backend return a different decision.

## 5. Rules the platform already enforces for flows

### 5.1 Tool allowlists

Keep each phase's allowlist small. A tool that is not allowed cannot run, so the allowlist is what stops the model booking before it has identified the caller. Prefer naming tools to `"*"`, and put only the tools a person could ask for at any time (`transfer_to_agent`, `hangup`) in `alwaysAllowedTools`.

### 5.2 Lines a business has signed off: `announce`

An `announce` is spoken as written, in the call's language, with `{slots.x}` rendered. Use it for a regulated disclosure, the hand-over line or the goodbye, where improvising is not acceptable. For the entry phase it is the opening line (`SessionConfig.OpeningText`); afterwards it is `VoiceSession.SpeakText`. It pre-empts and never queues. How exact it is depends on the engine: exact on doubao, best effort elsewhere (`docs/provider-extension.md` has the per-engine table).

Leave `announce` off phases where the model is answering the caller. It exists for the two ends of a call.

A deployment whose provider cannot be cued by text (doubao) refuses to **publish** a flow whose terminal phases have no `announce` (422 `TERMINAL_ANNOUNCE_REQUIRED`, naming the phases). Loading is unaffected. Writing an `announce` on every terminal phase makes a flow portable.

### 5.3 Closing phases and the two guards

Rule for a closing phase (`docs/design/02-ai-voice.md` §6): after "anything else?", a decline ("no", "that's all") means call `hangup` at once and never ask a second time; a follow-up or a new request is handled normally; a request to repeat is honoured. Write it into the phase instruction, as `plan_change`'s `done` does.

A model-spoken goodbye followed by `hangup` is unreliable ("say the closing line, then call hangup"). Some models, qwen among them, speak the line, end the turn without the tool call and leave the call open until dead air (issue [#147](https://github.com/rasonyang/ai-native-callcenter/issues/147)). Two shapes work:

- **A phase whose whole job is to close is a terminal phase with an `announce`.** The platform arms the ending before the line is said, speaks the line as written and hangs up after the caller has heard it; the model does no part of it. Reach it with a transition on the tool that finishes the work. Everything the line needs must be renderable in `announce` (`{slots.x}`, `{slots.<tool>.<result field>}`, `{slots.<tool>.args.<k>}`); when the line depends on a result, branch with conditioned transitions to separate terminal phases (the seeded `lead_qualification` goes to `finish_advisor` on `result.lead_grade EQ A` and to `finish_thanks` otherwise, `early_collections` ends in `promised` and `no_bill`).
- **In a phase where the conversation goes on, call the tool first.** Word the instruction as the `hangup` built-in does: call `hangup` immediately, do not say goodbye yourself, the closing line follows (`global.closingTarget`'s `announce`, or a phase-local `hangup` transition to a more specific terminal phase, as `early_collections` does for a caller who refuses to talk). A rule that says "apologize and call hangup" has the same defect as "say goodbye and call hangup".

`closingTarget` and the dead-air path remain backstops, not the normal ending.

The engine cannot see a caller's decline, only tool results and silence, so three guards back the rule up. All go to `global.closingTarget`, a terminal phase with its own `announce`:

- **Silence.** In a phase where no authored rule can fire on `NO_INPUT`, three consecutive silences move the call to `closingTarget`. A rule you wrote that can fire on `NO_INPUT` in that phase, however it is conditioned, switches this default off for the phase.
- **Tool-less replies.** `maxTurnsWithoutTool` bounds replies in one phase that follow the caller speaking and make no tool call. When the count exceeds it, the call moves to `closingTarget` after the caller has heard the reply. It resets on a tool call and on any phase change. Set it above the longest tool-less stretch any phase is meant to hold; the seeded flows use 10.
- **A goodbye with no hangup.** If the model said goodbye in words, made no tool call, and the caller then goes quiet, the dead-air path moves the call to `closingTarget` instead of asking the model anything (issue [#147](https://github.com/rasonyang/ai-native-callcenter/issues/147)). It does not apply to a campaign call, a terminal phase, an armed ending or a flow with no `closingTarget`.

These are backstops for a model that ignores the closing instruction, not the mechanism that normally ends a call. `maxTurns` with `fallbackTarget` is the equivalent backstop for a model that calls tools in a loop.

### 5.4 The wall clock

`maxDurationSec` counts from the moment the bot answers. At 80% a wrap-up line is added to the standing instructions; at 100% the call goes to the number's queue with `reason=SESSION_LIMIT` if that queue is open, otherwise to `closingTarget`. Absent means 900 seconds, not unlimited. Long-running flows (4.3) should keep this in mind. With no open queue and no `closingTarget`, the call gets a plain goodbye and a hangup.

### 5.5 Keep the standing prompt short

`persona` and `rules` are part of the standing instructions, which are re-pinned on every phase change, and `{slots.x}` is not rendered in them. Put in them only what holds for the whole call: who the bot is, the spoken style, the languages, and the handful of rules that apply everywhere (spoken output only, never invent numbers, confirm before a transaction). Everything else belongs in a phase instruction, a tool description, or the backend.

A rule that says "never invent plans, prices or order numbers; they must come from tool results" is worth keeping in `rules`, and is the reason a tool, not the prompt, should hold the facts.

## 6. What the DSL does not do yet

Say these to the business rather than working around them in the prompt.

| Need | Status |
| --- | --- |
| **Facts the caller states.** "I'm calling about order 1234", "my budget is 200". | Supported: a slot tool records the fact, see 2.8. Tool arguments are also kept as `<tool>.args.<k>`, and an outbound call's business data as `userData.<k>` (2.6). Still absent: a list of values (the slot keeps only the latest, so "all the orders looked up" needs your backend's memory), and a start-of-call lookup (issue [#84](https://github.com/rasonyang/ai-native-callcenter/issues/84)). Issue [#13](https://github.com/rasonyang/ai-native-callcenter/issues/13). |
| **Collecting digits, a keypad menu, masked secrets.** | Not supported. Keypad input is not a flow event. Today each key reaches the model as a text cue (`(The caller pressed 5 on their keypad.)`), interrupts the bot, and asks for a reply; the model assembles the digits and follows your prompt. That is not deterministic, and digits pass through the model, the transcript and the logs. Do not collect PINs or card numbers this way. Issue [#91](https://github.com/rasonyang/ai-native-callcenter/issues/91). |
| **Knowledge retrieval as a platform feature.** | Not provided. A knowledge tool is an ordinary HTTP tool (4.2) against a service you run. A neutral contract and store templates are planned in issue [#92](https://github.com/rasonyang/ai-native-callcenter/issues/92). |
| **Testing a flow with a model in the loop.** | `aicc flowsim` replays scripted scenarios deterministically (8). Tier 2 (a text model as the bot and another as the caller) is opt-in and spends money (8). |
| **Expressions, derived slots, arithmetic.** | By design the DSL has none; use a decision field (4.1). |
| **Asynchronous tools.** | A tool call is synchronous with a 5 second bound; there is no way to be notified when a background job finishes. Use submit and check, or a callback (4.3). |
| **Transitions on what the caller said.** | The only events are `TOOL_RESULT` and `NO_INPUT`. To branch on the caller's intent, have the model call a tool whose result carries the branch. |

## 7. A worked example

This is an illustrative flow in the seeded flows' style (a broadband plan change). It is not one of the seeded flows, and it calls endpoints (`/api/plan/*`, `/api/kb/search`) that `cmd/aicc-mockbackend` does not serve. It exists to show the patterns together: a decision tool, a knowledge tool, submit-and-check, a backend-decided transfer, and the closing rule.

The call flow: ask for the service number; `plan_check` decides (`OK_TO_CHANGE` goes to confirmation, `NEED_ID_VERIFY` goes to verification, `NEED_HUMAN` goes to the hand-over). Policy questions are answered through `kb_search` in the phases where they come up. A submitted change returns a ticket at once and `plan_status` reports its progress.

```json
{
  "id": "plan_change_decision_example",
  "specVersion": "v2",
  "entry": "plan_change",
  "initialNode": "welcome",
  "global": {
    "persona": {
      "en": "You are the voice assistant of the NovaNet Broadband hotline, handling plan changes.",
      "zh": "你是星联宽带客服热线的语音助手，为用户办理宽带套餐变更。"
    },
    "rules": {
      "en": [
        "This is a spoken phone conversation: one or two short sentences per reply, no lists or written formatting.",
        "Plan names, prices, order and ticket numbers must come from tool results. Never invent them.",
        "Before submitting a change, restate it and get the caller's explicit confirmation.",
        "When the caller clearly wants to end the call, call hangup immediately, then say goodbye."
      ],
      "zh": [
        "这是电话语音对话：每次只说一两句短句，不用列表或书面格式。",
        "套餐名称、价格、单号和工单号必须来自工具返回，严禁编造。",
        "提交变更前必须复述内容并获得用户明确确认。",
        "用户明确表达要结束通话，立即调用 hangup，工具返回后再道别。"
      ]
    },
    "fallbackTarget": "need_transfer",
    "closingTarget": "finish",
    "maxTurns": 30,
    "maxTurnsWithoutTool": 10,
    "alwaysAllowedTools": ["transfer_to_agent", "hangup"],
    "transitions": [
      {
        "on": "TOOL_RESULT",
        "tool": "transfer_to_agent",
        "condition": { "slot": "result.ok", "op": "EQ", "value": "1" },
        "target": "finish_transfer"
      },
      { "on": "TOOL_RESULT", "tool": "hangup", "target": "finish" }
    ]
  },
  "nodes": {
    "welcome": {
      "announce": {
        "en": "Welcome to the NovaNet Broadband hotline. Please tell me your 8-digit service number.",
        "zh": "星联宽带客服热线，语音助手为您服务。请报出您的8位宽带业务号码。"
      },
      "instruction": {
        "en": "When the caller gives the service number, call plan_check with it. For questions about prices or terms, call kb_search first.",
        "zh": "用户报出业务号码后，调用 plan_check。涉及价格或条款的问题，先调用 kb_search。"
      },
      "tools": ["plan_check", "kb_search"],
      "transitions": [
        {
          "on": "TOOL_RESULT",
          "tool": "plan_check",
          "condition": { "slot": "result.decision", "op": "EQ", "value": "OK_TO_CHANGE" },
          "target": "confirm"
        },
        {
          "on": "TOOL_RESULT",
          "tool": "plan_check",
          "condition": { "slot": "result.decision", "op": "EQ", "value": "NEED_ID_VERIFY" },
          "target": "verify"
        },
        {
          "on": "TOOL_RESULT",
          "tool": "plan_check",
          "condition": { "slot": "result.decision", "op": "EQ", "value": "NEED_HUMAN" },
          "target": "need_transfer"
        },
        {
          "on": "TOOL_RESULT",
          "tool": "plan_check",
          "condition": { "slot": "plan_check.calls", "op": "GTE", "value": 3 },
          "target": "need_transfer",
          "priority": 1
        }
      ]
    },
    "verify": {
      "instruction": {
        "en": "{slots.plan_check.spoken} Ask for the last 4 digits of the registered phone and call plan_verify with the service number. Two mismatches transfer to an agent.",
        "zh": "{slots.plan_check.spoken} 请用户提供登记电话后4位，调用 plan_verify 核验。连续两次不符会转人工。"
      },
      "tools": ["plan_verify"],
      "transitions": [
        {
          "on": "TOOL_RESULT",
          "tool": "plan_verify",
          "condition": { "slot": "result.match", "op": "EQ", "value": "1" },
          "target": "confirm"
        },
        {
          "on": "TOOL_RESULT",
          "tool": "plan_verify",
          "condition": { "slot": "plan_verify.calls", "op": "GTE", "value": 2 },
          "target": "need_transfer",
          "priority": 1
        }
      ]
    },
    "confirm": {
      "instruction": {
        "en": "The change is allowed. Say what the caller can choose: {slots.plan_check.spoken} Restate the chosen plan, get an explicit yes, then call plan_submit. For questions about terms, call kb_search.",
        "zh": "允许办理。可选内容：{slots.plan_check.spoken} 复述所选套餐，得到明确的“是”后调用 plan_submit。条款类问题先调用 kb_search。"
      },
      "tools": ["plan_submit", "kb_search", "take_message"],
      "transitions": [
        {
          "on": "TOOL_RESULT",
          "tool": "plan_submit",
          "condition": { "slot": "result.ok", "op": "EQ", "value": "1" },
          "target": "submitted"
        },
        {
          "on": "TOOL_RESULT",
          "tool": "plan_submit",
          "condition": { "slot": "result.ok", "op": "EQ", "value": "0" },
          "target": "need_transfer",
          "priority": 1
        }
      ]
    },
    "submitted": {
      "instruction": {
        "en": "The change was received as ticket {slots.plan_submit.ticket} and is being processed, not finished. Read the ticket number digit by digit and say it takes a little while. If the caller asks how it is going, call plan_status. Then ask if there is anything else. If the caller declines, call hangup immediately and do not ask again. If they ask you to repeat something, repeat it. If they have another request, handle it as usual.",
        "zh": "变更已受理，工单号 {slots.plan_submit.ticket}，正在处理，尚未完成。逐位读出工单号并说明需要一些时间。用户问进度时调用 plan_status。然后问还需要其它服务吗。对方说“没有了”“不用了”就立即调用 hangup，不要再问。对方请你重复就重复；还有别的需求就照常处理。"
      },
      "tools": ["plan_status", "kb_search"]
    },
    "need_transfer": {
      "instruction": {
        "en": "This cannot proceed via self-service. Apologise briefly, then immediately call transfer_to_agent. Put the decision and the reason in the summary.",
        "zh": "当前业务无法自助办理：向用户简短道歉，立即调用 transfer_to_agent，并把判定结果和原因写进 summary。"
      },
      "tools": []
    },
    "finish_transfer": {
      "announce": {
        "en": "Certainly. I am putting you through to an agent now. Please stay on the line.",
        "zh": "好的，正在为您转接人工客服，请不要挂机，稍等片刻。"
      },
      "instruction": {
        "en": "The caller is connected right after this line. Ask no further questions and do not say goodbye.",
        "zh": "说完这句就会接通，不要再问问题，也不要说再见。"
      },
      "tools": [],
      "isTerminal": true
    },
    "finish": {
      "announce": {
        "en": "Thank you for calling. Goodbye.",
        "zh": "感谢来电，再见。"
      },
      "instruction": {
        "en": "The line hangs up once this has been said. Ask no further questions.",
        "zh": "说完这句线路会自动挂断，不要再问问题。"
      },
      "tools": [],
      "isTerminal": true
    }
  },
  "tools": {
    "plan_check": {
      "description": {
        "en": "Decide whether this account may change plan. Returns a decision: OK_TO_CHANGE, NEED_ID_VERIFY or NEED_HUMAN, plus a short spoken explanation.",
        "zh": "判定该账户能否变更套餐。返回 OK_TO_CHANGE、NEED_ID_VERIFY 或 NEED_HUMAN，以及一句口语化说明。"
      },
      "parameters": {
        "type": "object",
        "properties": {
          "service_no": { "type": "string", "description": "8-digit service number" }
        },
        "required": ["service_no"]
      },
      "http": {
        "path": "/api/plan/check",
        "body": { "serviceNo": "{args.service_no}", "caller": "{slots.caller}" },
        "successWhen": { "path": "retCode", "equals": "000000" },
        "errorFrom": "retMsg",
        "result": {
          "decision": "data.decision",
          "reason": "data.reason",
          "spoken": "data.spoken"
        }
      }
    },
    "plan_verify": {
      "description": {
        "en": "Verify the last 4 digits of the registered phone.",
        "zh": "核验登记电话后4位。"
      },
      "parameters": {
        "type": "object",
        "properties": {
          "service_no": { "type": "string" },
          "phone_last4": { "type": "string" }
        },
        "required": ["service_no", "phone_last4"]
      },
      "http": {
        "path": "/api/plan/verify",
        "body": { "serviceNo": "{args.service_no}", "phoneLast4": "{args.phone_last4}" },
        "successWhen": { "path": "retCode", "equals": "000000" },
        "errorFrom": "retMsg",
        "result": { "match": "data.match" }
      }
    },
    "kb_search": {
      "description": {
        "en": "Search the plan and policy knowledge base. Call it for any question about prices, terms, fees or policy before answering; never answer such questions from memory.",
        "zh": "检索套餐与政策知识库。凡涉及价格、条款、费用或政策的问题，先调用本工具再回答，不要凭记忆作答。"
      },
      "parameters": {
        "type": "object",
        "properties": {
          "query": { "type": "string", "description": "The caller's question, in a few words" }
        },
        "required": ["query"]
      },
      "http": {
        "path": "/api/kb/search",
        "body": { "query": "{args.query}", "lang": "{slots.lang}" },
        "successWhen": { "path": "retCode", "equals": "000000" },
        "errorFrom": "retMsg",
        "result": { "answer": "data.answer", "found": "data.found" }
      }
    },
    "plan_submit": {
      "description": {
        "en": "Submit the confirmed plan change. Returns a ticket immediately; processing continues in the background. Only call after the caller explicitly confirms.",
        "zh": "提交已确认的套餐变更，立即返回工单号，后台继续处理。仅在用户明确确认后调用。"
      },
      "parameters": {
        "type": "object",
        "properties": {
          "service_no": { "type": "string" },
          "plan_id": { "type": "string", "enum": ["1", "2", "3"] }
        },
        "required": ["service_no", "plan_id"]
      },
      "http": {
        "path": "/api/plan/submit",
        "body": { "serviceNo": "{args.service_no}", "planId": "{args.plan_id}" },
        "successWhen": { "path": "retCode", "equals": "000000" },
        "errorFrom": "retMsg",
        "result": { "ticket": "data.ticket", "state": "data.state" }
      }
    },
    "plan_status": {
      "description": {
        "en": "Check the progress of a submitted plan change by its ticket.",
        "zh": "按工单号查询已提交变更的处理进度。"
      },
      "parameters": {
        "type": "object",
        "properties": { "ticket": { "type": "string" } },
        "required": ["ticket"]
      },
      "http": {
        "path": "/api/plan/status",
        "body": { "ticket": "{args.ticket}" },
        "successWhen": { "path": "retCode", "equals": "000000" },
        "errorFrom": "retMsg",
        "result": { "state": "data.state", "eta": "data.eta" }
      }
    }
  }
}
```

What to notice:

- **No rule is in the flow.** The thresholds behind `NEED_HUMAN` live in the backend. The `welcome` phase has four transitions and a three-valued vocabulary; the fourth (`plan_check.calls GTE 3`, priority 1) is the guard for a number that keeps failing.
- **`plan_check.spoken` is rendered into later instructions.** `{slots.plan_check.spoken}` works because `<tool>.<field>` slots persist after the next tool runs, where `result.*` would not.
- **A refusal is a branch.** A refused `plan_submit` (`result.ok` is `"0"`) goes to the hand-over rather than leaving the model to improvise.
- **The standing prompt is short.** The tariffs are not in it.
- **The closing rule is in `submitted`**, with `closingTarget` and `maxTurnsWithoutTool` behind it.

## 8. Verifying a flow

- **Load.** `aicc flowadd -file <flow>.json -did <number>` loads and publishes (the same slug updates and republishes). With `-did`, a number that does not exist is created **inbound only** (`allowInbound=true`, `allowOutbound=false`), and the command prints that; it cannot be the caller id of an AI outbound call (`POST /calls` with `AI_OUTBOUND` answers `422 DID_NOT_OUTBOUND`) until `allowOutbound` is set on it with `PUT /dids/{didId}` (`updateDID`). A number that already exists keeps all its flags and only its flow changes; the command prints the flags it kept. The same validation runs on `POST /flows` and `PUT /flows/{flowId}` (a spec the loader rejects is refused with 422 listing every problem). Publishing can additionally refuse with `TERMINAL_ANNOUNCE_REQUIRED` (5.2).
- **Backend.** Run `go run ./cmd/aicc-mockbackend` and point `AICC_BOT_BACKEND_BASE` at it to walk the seeded flows against deterministic fixtures (`docs/dev-stack.md`). For a new flow, write the equivalent handlers for your endpoints. Test the failure paths on purpose: a tool that times out, a `found: 0`, an unknown decision value.
- **Scenarios (the flow simulator).** `aicc flowsim` replays a scripted call against your flow on the real `flow.Engine` and `flow.Runtime`, with the flow's real HTTP tools served by the mock backend, and needs no database, switch or model:

  ```sh
  aicc flowsim -flow my_flow.json -scenario my_flow.scenarios.json      # add -v for every step, -only "name" for one scenario
  go test -race ./internal/flowsim/                                   # every seed flow against internal/seed/scenarios/
  ```

  A scenario fixes the call's metadata (`lang`, `caller`, `did`, `userData`), whether it is a campaign call (`isCampaign`: `record_lead` is offered, `transfer_to_agent` and `take_message` are withdrawn), the queues the deployment has, the answers of the built-ins (`actions`), and a list of `steps`. A step is exactly one event: `say` (a caller utterance), `keypress`, `exchange` (the caller speaks and the bot answers in words with no tool call), `noInput`, `botTurn` (`reply` or `interrupted`), `sessionLimit`, `call` with `args` (a tool the bot calls) or `batch` (several tools in one response; the last move wins). `repeat: N` runs a step N times. A step may carry `expect`: `phase`, `isOk`, `isRefused`, `result` (a subset of what the model receives), `slots` (a subset), `announce`, `instructionContains` (the rendered instruction, so a lost `{slots.x}` is caught). The scenario's own `expect` checks the whole call: `path` (phases entered, in order), `phase`, `ending` (`hangup`, `transfer_to_agent`, `take_message` or `none`), `transfer` (`queue`, `reason`), `toolsRun`, `slots`, `lead` (`intent`, `notes`, or `isUnrecorded` for a call that never recorded one). Unknown fields are an error, so a misspelt expectation cannot pass by checking nothing.

  ```json
  {"scenarios": [{
    "name": "caller disputes the bill",
    "lang": "en", "caller": "90000000", "queues": ["support-en"],
    "steps": [
      {"call": "bill_lookup", "expect": {"phase": "remind", "result": {"days_overdue": "21"}}},
      {"call": "dispute_record", "args": {"reason": "already paid"}, "expect": {"phase": "disputed"}},
      {"call": "transfer_to_agent", "args": {"queue": "support-en", "reason": "BILL_DISPUTE", "summary": "paid"}, "expect": {"phase": "finish_transfer"}}
    ],
    "expect": {"path": ["greet", "remind", "disputed", "finish_transfer"], "ending": "transfer_to_agent"}
  }]}
  ```

  What to cover: the main path, each failure (a tool answering `found: 0` or `ok: 0`, a refused transfer, an unknown queue), silence (`noInput` three times), the turn limits (`repeat` a tool past `maxTurns`; `exchange` past `maxTurnsWithoutTool`), and a tool called in a phase that does not allow it (`isRefused`). The run prints each scenario's phase path and ending, then the phases and phase-local transitions no scenario reached. Seed scenarios live in `internal/seed/scenarios/<flow>.json` (outside the embedded `flows/` directory), and `go test` requires a scenario file per seed flow that reaches every phase.

  The simulator checks the flow's logic: transitions, allowlists, slot rendering, built-in results, the guards. It is deterministic, so it cannot tell you what a model would say or whether it would call the tool you scripted; it does not exercise audio, barge-in, playback timing or provider quirks either. Passing it does not verify a call-path change.

- **Model in the loop (tier 2, opt-in, spends money).** A scenario with a `callerPersona` and a `model` block is also run by a text model playing the bot against a second text model playing the caller. The bot's model sees what a real call's model sees, taken from the same public `Runtime` methods the voice path uses: `Instructions()` (the platform rules, persona, rules and the phase) as the system prompt, `Tools()` as the tool list, `Announce()` as the line said as written, and every tool call goes through `Dispatch`, so phase moves, hints, allowlist refusals and the guards are the real ones. The caller model sees only its persona and what the bot said.

  ```json
  {"name": "model: english caller asks for a person", "lang": "en", "caller": "8005550100",
   "queues": ["support-en", "support-zh"],
   "callerPersona": {"goal": "Your internet is down; ask for a human agent.", "behaviour": "Slightly impatient, but polite."},
   "model": {"ending": "transfer_to_agent", "transfer": {"queue": "support-en"}, "phase": "finish_transfer", "passRate": 0.67}}
  ```

  `callerPersona` is `goal`, `persona` (facts the caller knows), `behaviour` (cooperative, declining, off-topic, or silent: the caller model answers `<silence>` and the engine gets a `NO_INPUT`) and an optional `maxTurns`. `model` states outcomes, because a model varies: `phase`, `ending`, `transfer`, `visits` (phases the call must enter), `avoids`, `slots`, and `passRate` (default 0.67). Each scenario is run `-runs N` times and passes when that share of runs meets every expectation. A scenario with `steps` still runs in tier 1; one with only a persona is skipped there.

  ```sh
  export ALIYUN_API_KEY=...            # exported, not read from .env
  AICC_LIVE_FLOWSIM=1 aicc flowsim -tier 2 -runs 3 -flow my_flow.json -scenario my_flow.scenarios.json
  AICC_LIVE_FLOWSIM=1 go test -count=1 -run Live -v ./internal/flowsim/    # every seed flow, 1 run each
  ```

  Settings, read by the simulator only (not by the server, so they are not in `.env.example`): `AICC_LIVE_FLOWSIM=1` (required; nothing is requested without it), `ALIYUN_API_KEY` (the qwen profile's key), `AICC_FLOWSIM_BOT_MODEL` (default `qwen-plus`), `AICC_FLOWSIM_CALLER_MODEL` (default `qwen-turbo`, the cheapest Qwen text tier, enough for a short persona), `AICC_FLOWSIM_ENDPOINT`. Without an endpoint it is the DashScope host of `AICC_PROVIDER_ENDPOINT` when that names one, else of the qwen realtime profile, with the OpenAI-compatible chat path. Flags override: `-bot-model`, `-caller-model`, `-endpoint`. Caps against a looping model: `-max-turns` (caller turns per run, default 20, and at most 6 model rounds inside one bot turn), `-max-calls` (model requests per invocation, default 300) and `-max-tokens` (default 1,000,000); the invocation stops at the first API error or when a cap is reached, and says what it did not run. Every tier 2 run (and every tier 1 scenario) gets a fresh in-process mock backend, so a booking one run makes is not found by the next. With `-backend URL` the simulator cannot reset the backend, so state carries between runs; it prints a warning when `-runs` is above 1 (tier 1: when the file has several scenarios). Output is stdout only: the phase path and ending of every run, the failure and transcript of a failing one (`-v` for all), and the requests and tokens spent.

  A text model is not the deployment's voice provider, so a tier 2 pass predicts a call only as far as the two resemble each other. It catches an instruction the model misreads, a tool description it misuses, and a branch it never reaches; it still cannot see audio, barge-in or provider quirks.
- **Live.** A flow is not verified until real calls have walked every branch, including the failure and transfer paths, and the logs and CDRs have been read. The number (DID) a caller dials picks the flow.

## 9. Checklist before publishing

- Is every business rule in the backend, with the flow branching on a field?
- Is every fact the bot may state coming from a tool result? Is anything long pasted into `persona`, `rules` or an `instruction`?
- Does each phase allow only the tools it needs, and does each tool's `description` say when to call it?
- Does every decision value the backend can return have a transition, and is there a fallback with a higher `priority` number?
- Does an operation that can exceed 5 seconds, or that a bot should not complete alone, have a submit-and-check, callback or hand-over path?
- Does every terminal phase have an `announce`, and does `closingTarget` name one of them?
- Does the closing phase tell the model to hang up on a decline?
- Is `maxTurnsWithoutTool` above the longest legitimate tool-less stretch?
- Does the flow avoid collecting secrets or digits through the model (6)?
- Are both languages written, with the same meaning?
