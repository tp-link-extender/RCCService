package main

import _ "embed"

// SOAP templates for the RCC instances. _TASK_ID, _EXPIRATION and the script placeholders are filled in per job
var (
	//go:embed templates/render.xml
	renderTemplate string

	//go:embed templates/host.xml
	hostTemplate string

	//go:embed templates/renew.xml
	renewTemplate string

	//go:embed templates/close.xml
	closeTemplate string
)
