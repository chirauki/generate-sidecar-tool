package main

import (
	"encoding/json"
	"fmt"
)

func marshalToString(data interface{}) string {
	js, err := json.MarshalIndent(data, "", "    ")
	if err != nil {
		return fmt.Sprintf("failed to marshal policy into json: %v", err)
	}
	return string(js)
}

func debugLogJSON(r *Runtime, data interface{}) {
	if !r.verbose {
		return
	}

	debug(marshalToString(data))
}
