package newapi

import (
	"context"
	"testing"

	"github.com/ShadowSmallBaby/ClawProxyHub/sdk"
	pb "github.com/ShadowSmallBaby/ClawProxyHub/sdk/proto/cphv1"
)

func TestFactoryKeepsVersionsIndependent(t *testing.T) {
	first, second := New("0.1.7"), New("0.1.8")
	for _, tc := range []struct {
		impl    sdk.Plugin
		version string
	}{{first, "0.1.7"}, {second, "0.1.8"}, {New(""), "dev"}} {
		response, err := tc.impl.Handshake(context.Background(), &pb.HandshakeRequest{ProtocolVersion: sdk.ProtocolVersion})
		if err != nil || response.GetManifest().GetVersion() != tc.version {
			t.Fatalf("factory handshake: %v, %v", response, err)
		}
		if _, ok := tc.impl.(sdk.HostAware); !ok {
			t.Fatal("factory lost host callbacks")
		}
	}
}
