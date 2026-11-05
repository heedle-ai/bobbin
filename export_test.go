package bobbin

import "time"

// SetHeartbeatEvery sets how often container steps heartbeat, for a test,
// and returns a function that puts it back.
func SetHeartbeatEvery(d time.Duration) func() {
	old := heartbeatEvery
	heartbeatEvery = d
	return func() { heartbeatEvery = old }
}
