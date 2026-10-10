//go:build !android

// 桌面入口保留 main.version 注入契约。
package main

import (
	"github.com/ShadowSmallBaby/ClawProxyHub/sdk"
	"github.com/ShadowSmallBaby/ClawProxyHubPlugins/plugins-go/loomy"
)

var version = "dev"

func main() { sdk.Serve(loomy.New(version)) }
