// +build ignore

package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"html/template"
	"io/ioutil"
	"os"
	"path"
)

type Manifest struct {
	Signatures []string `json:"process_signatures"`
	Name       *string  `json:"name"`
}

const codeTemplate = `// Code generated by go generate; DO NOT EDIT.
package procmatch

var DefaultCatalog IntegrationCatalog = IntegrationCatalog{ {{ range $_, $manifest := . }}
	Integration{
		Name: "{{$manifest.Name}}",
		Signatures: []string{ {{ range $_, $sig := $manifest.Signatures }}
			"{{$sig}}",{{end}}
		},
	},{{ end }}
}
`

func readManifest(raw []byte) (Manifest, bool) {
	m := Manifest{}
	_ = json.Unmarshal(raw, &m)
	return m, m.Signatures != nil && m.Name != nil
}

func failIf(err error, format string, args ...interface{}) {
	if err != nil {
		fmt.Fprintf(os.Stderr, format, args...)
		os.Exit(1)
	}
}

func main() {
	rootDir := os.Getenv("INTEGRATIONS_CORE_DIR")

	if len(rootDir) == 0 {
		fmt.Fprintln(os.Stderr, "Please set INTEGRATIONS_CORE_DIR env variable")
		os.Exit(1)
	}

	dirs, err := ioutil.ReadDir(rootDir)
	failIf(err, "An error occured listing directories in %s: %s", rootDir, err)

	manifests := []Manifest{}

	for _, dir := range dirs {
		if dir.IsDir() {
			// Ignore errors
			manifest, _ := ioutil.ReadFile(path.Join(rootDir, dir.Name(), "manifest.json"))
			decoded, ok := readManifest(manifest)
			if ok {
				manifests = append(manifests, decoded)
			}
		}
	}

	tmpl := template.New("catalog")
	tmpl, err = tmpl.Parse(codeTemplate)
	failIf(err, "Couldn't parse code template: %s", err)

	var buf bytes.Buffer

	err = tmpl.Execute(&buf, manifests)
	failIf(err, "Couldn't execute template: %s", err)

	err = ioutil.WriteFile("./default_catalog.go", buf.Bytes(), 0644)
	failIf(err, "Couldn't write file to disk: %s", err)

	fmt.Printf("%v entries generated !\n", len(manifests))
}
