package tests

import (
	"context"
	"github.com/ShadowSmallBaby/ClawProxyHub/sdk"
	pb "github.com/ShadowSmallBaby/ClawProxyHub/sdk/proto/cphv1"
	"github.com/ShadowSmallBaby/ClawProxyHubPlugins/plugins-go/chatjimmy"
	"github.com/ShadowSmallBaby/ClawProxyHubPlugins/plugins-go/cline"
	"github.com/ShadowSmallBaby/ClawProxyHubPlugins/plugins-go/codearts"
	"github.com/ShadowSmallBaby/ClawProxyHubPlugins/plugins-go/codebuff"
	"github.com/ShadowSmallBaby/ClawProxyHubPlugins/plugins-go/commandcode"
	"github.com/ShadowSmallBaby/ClawProxyHubPlugins/plugins-go/devin"
	"github.com/ShadowSmallBaby/ClawProxyHubPlugins/plugins-go/doubao"
	"github.com/ShadowSmallBaby/ClawProxyHubPlugins/plugins-go/gorkcli"
	"github.com/ShadowSmallBaby/ClawProxyHubPlugins/plugins-go/ima"
	"github.com/ShadowSmallBaby/ClawProxyHubPlugins/plugins-go/improvado"
	"github.com/ShadowSmallBaby/ClawProxyHubPlugins/plugins-go/joycode"
	"github.com/ShadowSmallBaby/ClawProxyHubPlugins/plugins-go/lobsterai"
	"github.com/ShadowSmallBaby/ClawProxyHubPlugins/plugins-go/loomy"
	"github.com/ShadowSmallBaby/ClawProxyHubPlugins/plugins-go/mimo"
	"github.com/ShadowSmallBaby/ClawProxyHubPlugins/plugins-go/mirasim"
	"github.com/ShadowSmallBaby/ClawProxyHubPlugins/plugins-go/newapi"
	"github.com/ShadowSmallBaby/ClawProxyHubPlugins/plugins-go/notion"
	"github.com/ShadowSmallBaby/ClawProxyHubPlugins/plugins-go/opencode"
	"github.com/ShadowSmallBaby/ClawProxyHubPlugins/plugins-go/postman"
	"github.com/ShadowSmallBaby/ClawProxyHubPlugins/plugins-go/puter"
	"github.com/ShadowSmallBaby/ClawProxyHubPlugins/plugins-go/qoder"
	"github.com/ShadowSmallBaby/ClawProxyHubPlugins/plugins-go/raccoon"
	"github.com/ShadowSmallBaby/ClawProxyHubPlugins/plugins-go/todofor"
	"github.com/ShadowSmallBaby/ClawProxyHubPlugins/plugins-go/trae"
	"github.com/ShadowSmallBaby/ClawProxyHubPlugins/plugins-go/warp"
	"github.com/ShadowSmallBaby/ClawProxyHubPlugins/plugins-go/workbuddy"
	"github.com/ShadowSmallBaby/ClawProxyHubPlugins/plugins-go/zcode"
	"testing"
)

// 平台入口依赖同一工厂契约；多个实例的版本不能互相覆盖。
func TestPluginFactories(t *testing.T) {
	for _, tc := range []struct {
		name string
		new  func(string) sdk.Plugin
	}{
		{"chatjimmy", chatjimmy.New},
		{"cline", cline.New},
		{"codearts", codearts.New},
		{"codebuff", codebuff.New},
		{"commandcode", commandcode.New},
		{"devin", devin.New},
		{"doubao", doubao.New},
		{"gorkcli", gorkcli.New},
		{"ima", ima.New},
		{"improvado", improvado.New},
		{"joycode", joycode.New},
		{"lobsterai", lobsterai.New},
		{"loomy", loomy.New},
		{"mimo", mimo.New},
		{"mirasim", mirasim.New},
		{"newapi", newapi.New},
		{"notion", notion.New},
		{"opencode", opencode.New},
		{"postman", postman.New},
		{"puter", puter.New},
		{"qoder", qoder.New},
		{"raccoon", raccoon.New},
		{"todofor", todofor.New},
		{"trae", trae.New},
		{"warp", warp.New},
		{"workbuddy", workbuddy.New},
		{"zcode", zcode.New},
	} {
		t.Run(tc.name, func(t *testing.T) {
			first, second, fallback := tc.new("1.2.3"), tc.new("4.5.6"), tc.new("")
			for _, instance := range []struct {
				impl    sdk.Plugin
				version string
			}{{first, "1.2.3"}, {second, "4.5.6"}, {fallback, "dev"}} {
				got, err := instance.impl.Handshake(context.Background(), &pb.HandshakeRequest{ProtocolVersion: sdk.ProtocolVersion})
				if err != nil || got.GetError().GetCode() != 0 || got.GetManifest().GetName() != tc.name || got.GetManifest().GetVersion() != instance.version {
					t.Fatalf("factory handshake: %v %v", got, err)
				}
			}
		})
	}
}
