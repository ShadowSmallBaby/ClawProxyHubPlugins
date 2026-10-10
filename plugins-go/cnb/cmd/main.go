//go:build !android

package main

import (
	"github.com/ShadowSmallBaby/ClawProxyHub/sdk"
	"github.com/ShadowSmallBaby/ClawProxyHubPlugins/plugins-go/cnb"
)

var version = "dev"

func main() { sdk.Serve(cnb.New(version)) }
