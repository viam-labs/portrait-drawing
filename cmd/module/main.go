// Package main runs the portrait-drawing Viam module, registering the drawer and stroke-generator services.
package main

import (
	"go.viam.com/rdk/module"
	"go.viam.com/rdk/resource"
	"go.viam.com/rdk/services/generic"

	"github.com/viam-labs/portrait-drawing/drawer"
	strokegenerator "github.com/viam-labs/portrait-drawing/stroke_generator"
)

func main() {
	module.ModularMain(
		resource.APIModel{API: generic.API, Model: drawer.Model},
		resource.APIModel{API: generic.API, Model: strokegenerator.Model},
	)
}
