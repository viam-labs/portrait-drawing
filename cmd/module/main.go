// Package main runs the portrait-drawing Viam module, registering the drawer service.
package main

import (
	"go.viam.com/rdk/module"
	"go.viam.com/rdk/resource"
	"go.viam.com/rdk/services/generic"

	"github.com/viam-labs/portrait-drawing/drawer"
)

func main() {
	module.ModularMain(
		resource.APIModel{API: generic.API, Model: drawer.Model},
	)
}
