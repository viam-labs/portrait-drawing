// Package main runs the portrait-drawing Viam module, registering the drawer, stroke-generator, text-writer and reception-queue services.
package main

import (
	"go.viam.com/rdk/module"
	"go.viam.com/rdk/resource"
	"go.viam.com/rdk/services/generic"

	"github.com/viam-labs/portrait-drawing/drawer"
	"github.com/viam-labs/portrait-drawing/queue"
	strokegenerator "github.com/viam-labs/portrait-drawing/stroke_generator"
	textwriter "github.com/viam-labs/portrait-drawing/text_writer"
)

func main() {
	module.ModularMain(
		resource.APIModel{API: generic.API, Model: drawer.Model},
		resource.APIModel{API: generic.API, Model: strokegenerator.Model},
		resource.APIModel{API: generic.API, Model: textwriter.Model},
		resource.APIModel{API: generic.API, Model: queue.Model},
	)
}
