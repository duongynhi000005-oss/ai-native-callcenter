// SPDX-License-Identifier: Apache-2.0

// Command aicc-mockbackend serves the business APIs the reference flows call,
// so a bot can be walked end to end without a real CRM behind it. The
// fixtures and their reasoning are in internal/mockbackend.
//
//	go run ./cmd/aicc-mockbackend -addr 127.0.0.1:8770
//	AICC_BOT_BACKEND_BASE=http://127.0.0.1:8770 /tmp/aicc
//
// POST /api/kb/secure-search is a second backend with a credential and a
// configurable delay: it answers only "Authorization: Bearer <token>", where
// the token is -kb-token (default: the AICC_MOCKBACKEND_KB_TOKEN environment
// variable; unset refuses every request), and only after the delayMs query
// parameter's milliseconds (0 to 15000; absent is 0). A flow tool reaches it
// with baseUrl, headers {"Authorization": "Bearer {secrets.KB}"} (and
// AICC_TOOL_SECRET_KB set to the same token in the app's environment),
// query {"delayMs": "3000"} and the body {"query": ..., "lang": ...}.
package main

import (
	"flag"
	"log"
	"net/http"
	"os"
	"time"

	"github.com/rasonyang/ai-native-callcenter/internal/mockbackend"
)

func main() {
	addr := flag.String("addr", "127.0.0.1:8770", "listen address")
	kbToken := flag.String("kb-token", os.Getenv("AICC_MOCKBACKEND_KB_TOKEN"),
		"bearer token /api/kb/secure-search accepts (default $AICC_MOCKBACKEND_KB_TOKEN; empty refuses all)")
	flag.Parse()

	log.Printf("mock backend on http://%s — point AICC_BOT_BACKEND_BASE at it", *addr)
	if *kbToken == "" {
		log.Printf("no -kb-token or AICC_MOCKBACKEND_KB_TOKEN: /api/kb/secure-search refuses every request")
	}
	srv := &http.Server{Addr: *addr, ReadHeaderTimeout: 5 * time.Second,
		Handler: mockbackend.NewWith(mockbackend.Options{Logf: log.Printf, KBToken: *kbToken})}
	log.Fatal(srv.ListenAndServe())
}
