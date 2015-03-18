package main

import (
	"fmt"
	"io/ioutil"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"
)

type Server struct {
	Env    []string // key=value
	GoPath string
	LogDir string
	PIDDir string

	User string
	Host string
	Port string

	set string

	client *ssh.Client
}

func (s Server) upload(info string) {
	s.initSetUp()

	var wg sync.WaitGroup
	ssh := fmt.Sprintf(`ssh -l %s -p %s`, s.User, strings.TrimLeft(s.Port, ":"))
	appName := cfg.App.Name
	// files upload
	wg.Add(len(cfg.App.Files))
	for _, src := range cfg.App.Files {
		go func(src string) {
			osrc := src
			defer func() {
				fmt.Printf("%s uploaded: %s\n", s, osrc)
				wg.Done()
			}()
			dst := fmt.Sprintf("%s@%s:harp/%s/files/%s", s.User, s.Host, appName, strings.Replace(src, "/", "_", -1))
			for _, path := range GoPaths {
				src = filepath.Join(path, "src", osrc)
				if fi, err := os.Stat(src); err != nil {
					src = ""
					continue
				} else if fi.IsDir() {
					src += "/"
				}

				break
			}
			if src == "" {
				exitf("failed to find %s from %s", osrc, GoPaths)
			}
			fmt.Printf("%s uploading: %s (from %s)\n", s, osrc, src)
			output, err := exec.Command("rsync", "-az", "--delete", "-e", ssh, src, dst).CombinedOutput()
			if err != nil {
				exitf("failed to sync %s: %s: %s", src, err, string(output))
			}
		}(src)
	}

	// binary upload
	wg.Add(1)
	go func() {
		defer func() {
			fmt.Printf("%s uploaded: binary %s\n", s, appName)
			wg.Done()
		}()
		fmt.Printf("%s uploading: binary %s\n", s, appName)
		dst := fmt.Sprintf("%s@%s:harp/%s/%s", s.User, s.Host, appName, appName)
		if debugf {
			fmt.Println("rsync", "-az", "--delete", "-e", ssh, "tmp/"+appName, dst)
		}
		output, err := exec.Command("rsync", "-az", "--delete", "-e", ssh, "tmp/"+appName, dst).CombinedOutput()
		if err != nil {
			exitf("failed to sync binary %s: %s: %s", appName, err, string(output))
		}
	}()

	// build info upload
	wg.Add(1)
	go func() {
		defer wg.Done()
		session := s.getSession()
		cmd := fmt.Sprintf("cat <<EOF > harp/%s/harp-build.info\n%s\nEOF", appName, info)
		output, err := session.CombinedOutput(cmd)
		if err != nil {
			exitf("failed to save build info: %s: %s", err, string(output))
		}
		session.Close()
	}()

	wg.Wait()
}

func (s Server) deploy() {
	if debugf {
		println("deplying", s.String())
	}

	var (
		logs                           []string
		beforeRsync, rsync, afterRsync string
	)

	beforeRsync += "set -e\n"

	if cfg.Hooks.Deploy.Before != "" {
		before, err := ioutil.ReadFile(cfg.Hooks.Deploy.Before)
		if err != nil {
			exitf("failed to read deploy before hook script: %s", err)
		}
		beforeRsync += string(before)
		beforeRsync += "\n"
	}

	gopath := s.getGoPath()

	rsync += fmt.Sprintf("mkdir -p %s/bin %s/src\n", gopath, gopath)

	// TODO: handle callback error
	for _, dst := range cfg.App.Files {
		src := fmt.Sprintf("harp/%s/files/%s", cfg.App.Name, strings.Replace(dst, "/", "_", -1))
		odst := dst
		dst = fmt.Sprintf("%s/src/%s", gopath, dst)

		var hasErr bool
		for _, path := range GoPaths {
			hasErr = false
			if fi, err := os.Stat(filepath.Join(path, "src", odst)); err != nil {
				hasErr = true
			} else if fi.IsDir() {
				src += "/"
				dst += "/"
			}
		}
		if hasErr {
			exitf("failed to find %s from %s", odst, GoPaths)
		}

		rsync += fmt.Sprintf("mkdir -p \"%s\"\n", filepath.Dir(dst))
		rsync += fmt.Sprintf("rsync -az --delete \"%s\" \"%s\"\n", src, dst)
	}

	rsync += fmt.Sprintf("cp harp/%s/harp-build.info %s/src/%s/\n", cfg.App.Name, gopath, cfg.App.ImportPath)
	rsync += fmt.Sprintf("rsync -az --delete harp/%[1]s/%[1]s %s/bin/%[1]s\n", cfg.App.Name, gopath)

	app := cfg.App
	log := fmt.Sprintf("$HOME/harp/%s/app.log", app.Name)
	pid := fmt.Sprintf("$HOME/harp/%s/app.pid", app.Name)
	logs = append(logs, log)
	afterRsync += fmt.Sprintf(`if [[ -f %[1]s ]]; then
	target=$(cat %[1]s);
	if ps -p $target > /dev/null; then
		kill -%[4]s $target; > /dev/null 2>&1;
	fi
fi
touch %[2]s
`, pid, log, app.Name, app.KillSig)

	envs := "GOPATH=" + gopath
	for k, v := range app.Envs {
		envs += fmt.Sprintf(" %s=%s", k, v)
	}
	args := strings.Join(app.Args, " ")
	afterRsync += fmt.Sprintf("cd %s/src/%s\n", gopath, app.ImportPath)
	afterRsync += fmt.Sprintf("%s nohup %s/bin/%s %s >> %s 2>&1 &\n", envs, gopath, app.Name, args, log)
	afterRsync += fmt.Sprintf("echo $! > %s\n", pid)

	if cfg.Hooks.Deploy.After != "" {
		after, err := ioutil.ReadFile(cfg.Hooks.Deploy.After)
		if err != nil {
			exitf("failed to read deploy after hook script: %s", err)
		}
		afterRsync += string(after)
		afterRsync += "\n"
	}

	script := beforeRsync + rsync + afterRsync
	if debugf {
		fmt.Printf("%s", script)
	}

	var output []byte
	session := s.getSession()
	defer session.Close()
	output, err := session.CombinedOutput(script)
	if err != nil {
		exitf("failed to exec %s: %s %s", script, string(output), err)
	}

	// TODO: save scripts(s) for kill app
	s.saveRestartScript(beforeRsync + afterRsync)
}

func (s Server) saveRestartScript(script string) {
	session := s.getSession()
	defer session.Close()
	cmd := fmt.Sprintf(`cat <<EOF > harp/%s/restart.sh
%s
EOF
chmod +x harp/%s/restart.sh
`, cfg.App.Name, script, cfg.App.Name)
	cmd = strings.Replace(cmd, "$", "\\$", -1)
	output, err := session.CombinedOutput(cmd)
	if err != nil {
		exitf("failed to save restart script on %s: %s: %s", s, err, string(output))
	}
}

func (s Server) getGoPath() string {
	var path = s.GoPath
	if path == "" {
		session := s.getSession()
		output, err := session.CombinedOutput("echo $GOPATH")
		if err != nil {
			fmt.Printf("echo $GOPATH on %s error: %s\n", s, err)
		}
		session.Close()
		path = strings.TrimSpace(string(output))
	}
	if path == "" {
		session := s.getSession()
		output, err := session.CombinedOutput("echo $HOME")
		if err != nil {
			fmt.Printf("echo $HOME on %s error: %s\n", s, err)
		}
		session.Close()
		path = strings.TrimSpace(string(output))
	}

	return path
}

func (s *Server) getSession() *ssh.Session {
	if s.client == nil {
		s.initClient()
	}

	session, err := s.client.NewSession()
	if err != nil {
		exitf("failed to get session to server %s@%s:%s: %s", s.User, s.Host, s.Port, err)
	}

	return session
}

// name@host:port
func (s Server) String() string {
	return fmt.Sprintf("%s@%s%s", s.User, s.Host, s.Port)
}

func (s *Server) initClient() {
	sock, err := net.Dial("unix", os.Getenv("SSH_AUTH_SOCK"))
	if err != nil {
		exitf("failed to dial unix SSH_AUTH_SOCK: %s", err)
	}
	signers, err := agent.NewClient(sock).Signers()
	if err != nil {
		exitf("failed to retrieve signers: %s", err)
	}
	auths := []ssh.AuthMethod{ssh.PublicKeys(signers...)}
	config := &ssh.ClientConfig{
		User: s.User,
		Auth: auths,
	}

	s.client, err = ssh.Dial("tcp", s.Host+s.Port, config)
	if err != nil {
		exitf("failed to dial %s: %s", s.Host+s.Port, err)
	}
}

func (s *Server) initSetUp() {
	if s.client == nil {
		s.initClient()
	}
	runCmd(s.client, fmt.Sprintf("mkdir -p harp/%s/files", cfg.App.Name))
}
