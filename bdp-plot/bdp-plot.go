package main

import (
	"flag"
	"io/ioutil"
	"log"
	"os"
	"os/exec"
	"text/template"
)

const tpl = `
set terminal png size 800,600
set output "{{.OutputPath}}"
# set logscale
# set xrange [8e5:3e6]
# set yrange [5e4:5e5]

plot "{{.InputPath}}" using 1:2:0 with points pointtype 1 pointsize 1 palette
`

var args struct {
	InputPath  string
	OutputPath string
}

func init() {
	log.SetFlags(0)
	flag.StringVar(&args.InputPath, "i", "", "input path (csv)")
	flag.StringVar(&args.OutputPath, "o", "", "output path (png)")
	flag.Parse()
	if args.InputPath == "" {
		log.Fatal("-i ?")
	}
	if args.OutputPath == "" {
		log.Fatal("-o ?")
	}
}

func main() {
	t := template.Must(template.New("gnuplot").Parse(tpl))

	tempfile, err := ioutil.TempFile("", "gnuplot-tpl")
	if err != nil {
		log.Fatal(err)
	}
	log.Println(tempfile.Name())
	defer os.Remove(tempfile.Name())

	if err = t.Execute(tempfile, args); err != nil {
		log.Fatal(err)
	}
	if err = tempfile.Close(); err != nil {
		log.Fatal(err)
	}
	if content, err := ioutil.ReadFile(tempfile.Name()); err != nil {
		log.Fatal(err)
	} else {
		log.Println(string(content))
	}
	cmd := exec.Command("gnuplot", tempfile.Name())
	output, err := cmd.CombinedOutput()
	if err != nil {
		log.Fatal(err)
	}
	log.Println(output)
}
