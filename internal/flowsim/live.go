// SPDX-License-Identifier: Apache-2.0

package flowsim

import (
	"fmt"
	"net/url"
	"strings"

	"github.com/rasonyang/ai-native-callcenter/internal/provider"
)

// Settings of a real tier 2 run. They are read only by the simulator (the
// aicc flowsim command and the live test), never by the server, so they are
// not in internal/config or .env.example; docs/flow-authoring.md section 8
// lists them.
const (
	// EnvLive must be "1" for any real model call, as the live provider tests
	// need AICC_LIVE_PROVIDER_TEST.
	EnvLive = "AICC_LIVE_FLOWSIM"
	// EnvEndpoint overrides the chat API base URL.
	EnvEndpoint = "AICC_FLOWSIM_ENDPOINT"
	// EnvBotModel and EnvCallerModel override the two models.
	EnvBotModel    = "AICC_FLOWSIM_BOT_MODEL"
	EnvCallerModel = "AICC_FLOWSIM_CALLER_MODEL"

	// DefaultBotModel plays the bot: the flagship Qwen text tier, the nearest
	// text model to the qwen realtime deployment (owner decision, issue #85).
	DefaultBotModel = "qwen-plus"
	// DefaultCallerModel plays the caller: the cheapest Qwen text tier. The
	// caller only has to stay in a short persona and say one or two sentences,
	// which does not need the bot's model, and it is called as often as the
	// bot is.
	DefaultCallerModel = "qwen-turbo"

	// chatPath is DashScope's OpenAI-compatible chat API under the host the
	// qwen realtime profile already names.
	chatPath = "/compatible-mode/v1"
)

// Live is what a real tier 2 run needs.
type Live struct {
	// BaseURL is the chat API base, ending in the version path.
	BaseURL string
	// APIKey is the Qwen key, read from the variable the qwen profile names.
	APIKey      string
	BotModel    string
	CallerModel string
}

// ResolveLive reads the live settings. The flag values win over the
// environment; an empty one falls through.
//
// The endpoint is not a new setting where one exists: it is the host of
// AICC_PROVIDER_ENDPOINT when that names a DashScope host (an international
// or workspace deployment), else the host of the qwen realtime profile, with
// the chat path in place of the realtime one. The key is the qwen profile's
// own, ALIYUN_API_KEY.
func ResolveLive(getenv func(string) string, endpointFlag, botFlag, callerFlag string) (Live, error) {
	if getenv(EnvLive) != "1" {
		return Live{}, fmt.Errorf("tier 2 spends real money on model calls; set %s=1 to allow it", EnvLive)
	}
	profile := provider.QwenProfile()
	key := getenv(profile.APIKeyEnv)
	if key == "" {
		return Live{}, fmt.Errorf("%s is not set", profile.APIKeyEnv)
	}

	source := firstNonEmpty(endpointFlag, getenv(EnvEndpoint))
	if source == "" {
		if configured := getenv("AICC_PROVIDER_ENDPOINT"); strings.Contains(configured, "dashscope") {
			source = configured
		} else {
			source = profile.Endpoint
		}
	}
	base, err := chatBase(source)
	if err != nil {
		return Live{}, err
	}
	return Live{
		BaseURL:     base,
		APIKey:      key,
		BotModel:    firstNonEmpty(botFlag, getenv(EnvBotModel), DefaultBotModel),
		CallerModel: firstNonEmpty(callerFlag, getenv(EnvCallerModel), DefaultCallerModel),
	}, nil
}

// chatBase turns an endpoint into the chat API base. An endpoint that already
// carries a path (a full chat base given explicitly) is kept; a bare host or a
// realtime URL gets the compatible-mode path, over https.
func chatBase(endpoint string) (string, error) {
	u, err := url.Parse(endpoint)
	if err != nil || u.Host == "" {
		return "", fmt.Errorf("the chat endpoint %q is not a URL", endpoint)
	}
	switch u.Scheme {
	case "wss":
		u.Scheme = "https"
	case "ws":
		u.Scheme = "http"
	}
	if strings.Contains(u.Path, "/realtime") || u.Path == "" || u.Path == "/" {
		u.Path = chatPath
	}
	u.RawQuery, u.Fragment = "", ""
	return strings.TrimSuffix(u.String(), "/"), nil
}

// Chats builds the bot and caller clients, both capped by one shared budget.
func (l Live) Chats(maxCalls, maxTokens int) (*Budget, ModelConfig) {
	budget := NewBudget(&HTTPChat{BaseURL: l.BaseURL, APIKey: l.APIKey}, maxCalls, maxTokens)
	return budget, ModelConfig{Bot: budget, Caller: budget, BotModel: l.BotModel, CallerModel: l.CallerModel}
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}
