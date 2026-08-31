package main

import (
	"context"
	"flag"
	"log"
	"os/signal"
	"syscall"
	"time"

	"github.com/janpuc/t3-code-operator/internal/smbserver"
)

type options struct {
	username        string
	shareName       string
	serverIdentity  string
	passwordFile    string
	workspacePath   string
	stateDirectory  string
	unixUser        string
	port            int
	readOnly        bool
	pollInterval    time.Duration
	smbdBinary      string
	smbpasswdBinary string
	netBinary       string
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, parseOptions()); err != nil {
		log.Fatal(err)
	}
}

func parseOptions() options {
	var value options
	flag.StringVar(&value.username, "username", "t3", "SMB client username")
	flag.StringVar(&value.shareName, "share-name", "workspace", "SMB share name")
	flag.StringVar(&value.serverIdentity, "server-identity", "", "stable SMB server identity")
	flag.StringVar(&value.passwordFile, "password-file", "/var/run/secrets/t3-smb/password", "SMB password file")
	flag.StringVar(&value.workspacePath, "workspace", "/workspace", "workspace directory")
	flag.StringVar(&value.stateDirectory, "state-directory", "/var/lib/t3-smb", "ephemeral Samba state directory")
	flag.StringVar(&value.unixUser, "unix-user", "node", "runtime UNIX user")
	flag.IntVar(&value.port, "port", smbserver.DefaultPort, "unprivileged SMB container port")
	flag.BoolVar(&value.readOnly, "read-only", false, "export the workspace read-only")
	flag.DurationVar(&value.pollInterval, "password-poll-interval", smbserver.DefaultPollInterval, "Secret password poll interval")
	flag.StringVar(&value.smbdBinary, "smbd-binary", "/usr/sbin/smbd", "Samba server binary")
	flag.StringVar(&value.smbpasswdBinary, "smbpasswd-binary", "/usr/bin/smbpasswd", "Samba password binary")
	flag.StringVar(&value.netBinary, "net-binary", "/usr/bin/net", "Samba administration binary")
	flag.Parse()
	return value
}

func run(ctx context.Context, value options) error {
	server, err := smbserver.New(smbserver.Config{
		Username:        value.username,
		ShareName:       value.shareName,
		ServerIdentity:  value.serverIdentity,
		PasswordFile:    value.passwordFile,
		WorkspacePath:   value.workspacePath,
		StateDirectory:  value.stateDirectory,
		UnixUser:        value.unixUser,
		Port:            value.port,
		ReadOnly:        value.readOnly,
		PollInterval:    value.pollInterval,
		SMBDBinary:      value.smbdBinary,
		SMBPasswdBinary: value.smbpasswdBinary,
		NetBinary:       value.netBinary,
	})
	if err != nil {
		return err
	}
	return server.Run(ctx)
}
