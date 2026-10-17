package httpapi_test

import (
	"encoding/json"
	"net/http"
	"testing"
)

// TestPostSubscriber_KeepsTimezoneAndData: POST /v1/subscribers stores the
// timezone and custom data it is sent, which it used to drop silently, and a
// later POST that omits them leaves them as they were.
func TestPostSubscriber_KeepsTimezoneAndData(t *testing.T) {
	srv, auth := startAgentServer(t)
	decode := func(body string) (tz, data string) {
		var s struct {
			Timezone string
			Data     json.RawMessage
		}
		if err := json.Unmarshal([]byte(body), &s); err != nil {
			t.Fatalf("decode %s: %v", body, err)
		}
		return s.Timezone, string(s.Data)
	}

	status, body := doPost(t, srv.URL+"/v1/subscribers", auth, `{"subscriberId":"tz1","timezone":"Asia/Tokyo","data":{"plan":"pro"}}`)
	if status != http.StatusCreated {
		t.Fatalf("create: %d %s", status, body)
	}
	if tz, data := decode(body); tz != "Asia/Tokyo" || data != `{"plan":"pro"}` {
		t.Fatalf("created timezone=%q data=%s", tz, data)
	}

	status, body = doPost(t, srv.URL+"/v1/subscribers", auth, `{"subscriberId":"tz1","firstName":"Ann"}`)
	if status != http.StatusCreated {
		t.Fatalf("update: %d %s", status, body)
	}
	if tz, data := decode(body); tz != "Asia/Tokyo" || data != `{"plan":"pro"}` {
		t.Fatalf("after a POST without them: timezone=%q data=%s, want both kept", tz, data)
	}
}
