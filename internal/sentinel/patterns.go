package sentinel

import "time"

// Built-in path predicates, exported so custom patterns can reuse them.
var (
	// SSHPath matches anything under an .ssh directory.
	SSHPath = AnyPath(PathUnder(".ssh"), BaseIn("authorized_keys", "authorized_keys2"))

	// ShellRCPath matches shell start-up files.
	ShellRCPath = AnyPath(
		BaseIn(".bashrc", ".bash_profile", ".bash_login", ".bash_logout", ".profile", ".zshrc", ".zshenv",
			".zprofile", ".zlogin", ".kshrc", ".cshrc", ".tcshrc", ".inputrc", "bash.bashrc", "microsoft.powershell_profile.ps1"),
		HasSuffix("/etc/profile", ".config/fish/config.fish"),
		PathUnder("/etc/profile.d", ".config/fish/conf.d"),
	)

	// CIConfigPath matches CI/CD pipeline definitions.
	CIConfigPath = AnyPath(
		PathUnder(".github/workflows", ".github/actions", ".circleci", ".buildkite", ".gitea/workflows", ".forgejo/workflows", ".woodpecker"),
		BaseIn(".gitlab-ci.yml", ".gitlab-ci.yaml", "jenkinsfile", "azure-pipelines.yml", ".travis.yml",
			"bitbucket-pipelines.yml", ".drone.yml", "cloudbuild.yaml", "buildspec.yml", "codemagic.yaml", ".woodpecker.yml"),
	)

	// GitHookPath matches git hook scripts.
	GitHookPath = AnyPath(PathUnder(".git/hooks", ".husky"))

	// SecretPath matches files that conventionally hold credentials.
	SecretPath = ExceptPath(
		AnyPath(
			BaseIn(".env", ".netrc", "_netrc", ".npmrc", ".pypirc", ".git-credentials", ".pgpass", ".htpasswd",
				"id_rsa", "id_dsa", "id_ecdsa", "id_ed25519", "id_ecdsa_sk", "id_ed25519_sk",
				"credentials", "credentials.json", "secrets.json", "secrets.yml", "secrets.yaml",
				"service-account.json", "kubeconfig", "terraform.tfvars", "vault-token", ".vault-token"),
			BasePrefix(".env."),
			HasSuffix(".pem", ".key", ".p12", ".pfx", ".jks", ".keystore", ".kdbx", ".ovpn", ".tfstate"),
			PathUnder(".aws", ".gnupg", ".kube", ".config/gcloud", ".azure", ".docker", ".password-store"),
		),
		BaseIn(".env.example", ".env.sample", ".env.template", ".env.dist", ".env.defaults"),
		HasSuffix(".pub"),
	)
)

// netTools are commands that talk to the network.
var netTools = []string{"curl", "wget", "nc", "ncat", "netcat", "socat", "scp", "sftp", "ftp", "telnet", "rsync",
	"invoke-webrequest", "iwr", "invoke-restmethod", "irm", "http", "https", "aria2c"}

// loopback matches targets that stay on this machine.
var loopback = Contains("localhost", "127.0.0.1", "[::1]", "0.0.0.0", "host.docker.internal")

// BuiltinPatterns returns the default dangerous-sequence patterns. The
// returned slice is fresh and may be modified.
func BuiltinPatterns() []Pattern {
	netEgress := Any(
		All(MatchFunc(func(in *Input) bool { return in.Kind == KindNet }), Not(loopback)),
		All(MatchFunc(func(in *Input) bool { return in.Kind != KindNet }), CmdWord(netTools...), Not(loopback)),
	)
	return []Pattern{
		{
			Name:        "download-chmod-exec",
			Description: "a downloaded file was made executable and then run",
			Severity:    SeverityCritical,
			Action:      ActionRequireApproval,
			Within:      15 * time.Minute,
			Steps: []Step{
				{Name: "download", Kinds: Kinds(KindExec, KindToolCall), Match: Shell(ShellDownload), Capture: CaptureDownload},
				{Name: "chmod +x", Kinds: Kinds(KindExec, KindToolCall), Match: All(CmdWord("chmod"), Contains("+x", " 7", " 5", "a+rx", "u+rx")), Bind: true},
				{Name: "execute", Kinds: Kinds(KindExec, KindToolCall), Match: Not(CmdWord("chmod", "curl", "wget", "rm", "cat", "less", "head", "tail", "file", "ls", "stat", "sha256sum", "shasum", "md5sum")), Bind: true},
			},
		},
		{
			Name:        "pipe-to-shell",
			Description: "a download was piped straight into an interpreter",
			Severity:    SeverityCritical,
			Action:      ActionRequireApproval,
			Within:      time.Minute,
			Steps:       []Step{{Name: "curl | sh", Kinds: Kinds(KindExec, KindToolCall), Match: Shell(ShellPipeToShell)}},
		},
		{
			Name:        "ssh-write",
			Description: "write to an SSH directory (keys, authorized_keys, config)",
			Severity:    SeverityCritical,
			Action:      ActionRequireApproval,
			Within:      time.Minute,
			Steps:       []Step{{Name: "write ~/.ssh", Kinds: Kinds(KindFileWrite, KindFileDelete, KindExec, KindToolCall), Match: Writes(SSHPath)}},
		},
		{
			Name:        "shell-rc-write",
			Description: "write to a shell start-up file (persistence)",
			Severity:    SeverityHigh,
			Action:      ActionRequireApproval,
			Within:      time.Minute,
			Steps:       []Step{{Name: "write rc file", Kinds: Kinds(KindFileWrite, KindFileDelete, KindExec, KindToolCall), Match: Writes(ShellRCPath)}},
		},
		{
			Name:        "git-hook-write",
			Description: "write to a git hook, which runs on every commit or checkout",
			Severity:    SeverityHigh,
			Action:      ActionRequireApproval,
			Within:      time.Minute,
			Steps: []Step{{Name: "write hook", Kinds: Kinds(KindFileWrite, KindFileDelete, KindExec, KindToolCall),
				Match: Any(Writes(GitHookPath), All(CmdWord("git"), Contains("core.hookspath")))}},
		},
		{
			Name:        "ci-config-write",
			Description: "change to a CI/CD pipeline definition",
			Severity:    SeverityMedium,
			Action:      ActionRequireApproval,
			Within:      time.Minute,
			Steps:       []Step{{Name: "write CI config", Kinds: Kinds(KindFileWrite, KindFileDelete, KindExec, KindToolCall), Match: Writes(CIConfigPath)}},
		},
		{
			Name:        "secret-exfiltration-inline",
			Description: "one command both reads a credential file and sends data over the network",
			Severity:    SeverityCritical,
			Within:      time.Minute,
			Steps: []Step{{Name: "read secret and send", Kinds: Kinds(KindExec, KindToolCall),
				Match: All(CmdWord(netTools...), Touches(SecretPath), Not(loopback))}},
		},
		{
			Name:        "secret-exfiltration",
			Description: "a credential file was read and then a network request was made",
			Severity:    SeverityHigh,
			Action:      ActionRequireApproval,
			Within:      5 * time.Minute,
			Steps: []Step{
				{Name: "read secret", Kinds: Kinds(KindFileRead, KindExec, KindToolCall), Match: Touches(SecretPath)},
				{Name: "network egress", Kinds: Kinds(KindNet, KindExec, KindToolCall), Match: netEgress},
			},
		},
		{
			Name:        "catastrophic-delete",
			Description: "recursive delete of a root-like path, or a disk-level overwrite",
			Severity:    SeverityCritical,
			Action:      ActionRequireApproval,
			Within:      time.Minute,
			Steps:       []Step{{Name: "rm -rf /", Kinds: Kinds(KindExec, KindToolCall), Match: Shell(ShellCatastrophicDelete)}},
		},
	}
}
