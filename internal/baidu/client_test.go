package baidu

import (
	"encoding/json"
	"testing"
)

func TestAPIResponseAcceptsStringAndNumericRequestID(t *testing.T) {
	tests := []struct {
		name string
		body string
		want string
	}{
		{name: "string", body: `{"errno":0,"request_id":"1234567890123456789"}`, want: "1234567890123456789"},
		{name: "number", body: `{"errno":0,"request_id":1234567890123456789}`, want: "1234567890123456789"},
		{name: "null", body: `{"errno":0,"request_id":null}`, want: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var response apiResponse
			if err := json.Unmarshal([]byte(tt.body), &response); err != nil {
				t.Fatal(err)
			}
			if string(response.RequestID) != tt.want {
				t.Fatalf("request_id = %q, want %q", response.RequestID, tt.want)
			}
		})
	}
}
