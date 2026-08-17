package dispatch

import (
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
)

// TestHandleMethod_MalformedJSON_NeverPanics adversarially probes every
// declared method with malformed/adversarial JSON request bytes,
// confirming HandleMethod always returns a typed error envelope instead
// of panicking, per the fail-loud principle: a malformed request from a
// buggy or malicious host must produce a clean typed error, not crash
// the plugin process.
func TestHandleMethod_MalformedJSON_NeverPanics(t *testing.T) {
	methodsWithJSONBodies := []string{
		pluginabi.MethodAuthParse,
		pluginabi.MethodAuthLoginStart,
		pluginabi.MethodAuthLoginPoll,
		pluginabi.MethodAuthRefresh,
		pluginabi.MethodModelStatic,
		pluginabi.MethodModelForAuth,
		pluginabi.MethodExecutorExecute,
		pluginabi.MethodExecutorExecuteStream,
		pluginabi.MethodExecutorCountTokens,
	}

	adversarialPayloads := [][]byte{
		nil,
		{},
		[]byte("not json at all"),
		[]byte(`{`),
		[]byte(`{"unexpected": "shape", "nested": {"deeply": [1,2,3,{"a":"b"}]}}`),
		[]byte(`null`),
		[]byte(`[]`),
		[]byte(`"just a string"`),
		[]byte(`{"payload": "` + string(make([]byte, 10000)) + `"}`),
	}

	for _, method := range methodsWithJSONBodies {
		for i, payload := range adversarialPayloads {
			func() {
				defer func() {
					if r := recover(); r != nil {
						t.Errorf("HandleMethod(%q, payload#%d) panicked: %v", method, i, r)
					}
				}()
				raw, err := HandleMethod(method, payload)
				if err != nil {
					// A transport-level Go error is acceptable (still no
					// panic); the important invariant is no crash.
					return
				}
				if len(raw) == 0 {
					t.Errorf("HandleMethod(%q, payload#%d) returned empty response with nil error", method, i)
				}
			}()
		}
	}
}

// TestHandleMethod_EmptyMethodName_NoPanic probes an empty/unusual method
// name string.
func TestHandleMethod_EmptyMethodName_NoPanic(t *testing.T) {
	for _, method := range []string{"", " ", "plugin.register\x00extra", "🎉"} {
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Errorf("HandleMethod(%q, nil) panicked: %v", method, r)
				}
			}()
			_, _ = HandleMethod(method, nil)
		}()
	}
}
