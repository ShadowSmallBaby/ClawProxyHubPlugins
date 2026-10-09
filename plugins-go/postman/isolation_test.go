package postman

// F06 隔离验证：不同账号/实例/调用方同首条文本不得命中同一会话。
import (
	"testing"

	pb "github.com/ShadowSmallBaby/ClawProxyHub/sdk/proto/cphv1"
)

func mkReq(instance int64, account, caller, text string) *pb.ChatRequest {
	return &pb.ChatRequest{
		Messages:   []*pb.EnvelopeMessage{{Role: "user", Text: text}},
		Credential: &pb.CredentialBlob{InstanceId: instance, AccountId: account, Blob: []byte(`{"api_key":"k"}`)},
		Extra:      map[string]string{"cph.caller_id": caller},
	}
}

func TestScopedThreadKeyIsolation(t *testing.T) {
	a := mkReq(1, "acct-a", "caller1", "hello")
	b := mkReq(1, "acct-b", "caller1", "hello")
	if scopedThreadKey(a) == scopedThreadKey(b) {
		t.Fatal("different accounts share thread key")
	}
	c := mkReq(2, "acct-a", "caller1", "hello")
	if scopedThreadKey(a) == scopedThreadKey(c) {
		t.Fatal("different instances share thread key")
	}
	d := mkReq(1, "acct-a", "caller2", "hello")
	if scopedThreadKey(a) == scopedThreadKey(d) {
		t.Fatal("different callers share thread key")
	}
	e := mkReq(1, "acct-a", "caller1", "hello")
	if scopedThreadKey(a) != scopedThreadKey(e) {
		t.Fatal("same request shape must be stable")
	}
}
