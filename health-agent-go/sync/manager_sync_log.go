package sync

import (
	"fmt"
	"net/http"
	stdsync "sync"
)

// managerSyncOutcomeByKey stores last logged outcome per logical key (e.g. "callback", "inactive:<dep>", "sync-intervals")
// so we only emit WARN/ERROR when the outcome changes, and INFO once when recovering to OK.
var managerSyncOutcomeByKey stdsync.Map

// managerHTTPOutcomeSig returns a stable string for HTTP result classification (dedupe key).
func managerHTTPOutcomeSig(err error, resp *http.Response) string {
	if err != nil {
		return "neterr"
	}
	if resp == nil {
		return "nilresp"
	}
	sc := resp.StatusCode
	if sc >= 200 && sc < 300 {
		return "ok"
	}
	if sc >= 300 && sc < 400 {
		return fmt.Sprintf("redirect:%d", sc)
	}
	return fmt.Sprintf("http:%d", sc)
}

func noteManagerSyncOutcomeString(key, cur string, logRecover func(prev string), logBad func()) {
	prevAny, _ := managerSyncOutcomeByKey.Load(key)
	prev, _ := prevAny.(string)
	if cur == prev {
		return
	}
	if prev != "" && prev != "ok" && cur == "ok" {
		if logRecover != nil {
			logRecover(prev)
		}
	} else if cur != "ok" {
		if logBad != nil {
			logBad()
		}
	}
	managerSyncOutcomeByKey.Store(key, cur)
}

func noteManagerHTTPOutcome(key string, err error, resp *http.Response, logRecover func(prev string), logBad func()) {
	noteManagerSyncOutcomeString(key, managerHTTPOutcomeSig(err, resp), logRecover, logBad)
}
