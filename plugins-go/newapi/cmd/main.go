//go:build !android

// 桌面入口保留打包器的 main.version 注入契约。
package main

import (
	"github.com/ShadowSmallBaby/ClawProxyHub/sdk"
	"github.com/ShadowSmallBaby/ClawProxyHubPlugins/plugins-go/newapi"
)

var version = "dev"

func main() { sdk.Serve(newapi.New(version)) }
