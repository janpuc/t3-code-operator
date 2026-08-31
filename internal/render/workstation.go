package render

import (
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"unicode"
)

const (
	machineInfoPath     = "/data/t3-coded/machine-info"
	gitConfigPath       = "/data/home/.gitconfig"
	githubConfigPath    = "/data/t3-coded/gh/config.yml"
	githubHostsPath     = "/data/t3-coded/gh/hosts.yml"
	gitPrivateKeyPath   = "/data/home/.ssh/id_signing"
	gitPublicKeyPath    = "/data/home/.ssh/id_signing.pub"
	gitAllowedUsersPath = "/data/home/.ssh/allowed_signers"
)

var githubUserPattern = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9-]{0,37}[A-Za-z0-9])?$`)

func renderWorkstationFiles(input ResolvedWorkstation) ([]FileTarget, error) {
	files := make([]FileTarget, 0, 7)
	if input.MachineInfo != nil {
		if err := validateSingleLine("machineInfo.prettyHostname", input.MachineInfo.PrettyHostname, true); err != nil {
			return nil, err
		}
		files = append(files, FileTarget{
			Scope:   FileScopeWorkstation,
			Path:    machineInfoPath,
			Mode:    WriteModeReplace,
			Format:  FileFormatText,
			Content: "PRETTY_HOSTNAME=" + quoteConfigurationValue(input.MachineInfo.PrettyHostname) + "\n",
			Apply:   workstationFileApplyPolicy(),
		})
	}

	if input.Git == nil {
		return files, nil
	}
	gitFiles, err := renderGitFiles(input.Namespace, *input.Git)
	if err != nil {
		return nil, err
	}
	return append(files, gitFiles...), nil
}

func renderGitFiles(namespace string, git GitConfiguration) ([]FileTarget, error) {
	if err := validateSingleLine("git.userName", git.UserName, false); err != nil {
		return nil, err
	}
	if err := validateSingleLine("git.userEmail", git.UserEmail, false); err != nil {
		return nil, err
	}
	if git.CredentialSecretRef != nil {
		if !githubUserPattern.MatchString(git.GitHubUser) {
			return nil, validationError("git.githubUser", "is required and must be a GitHub user name when credentials are configured")
		}
		if err := validateSecretReference(namespace, "git.credentialSecretRef", *git.CredentialSecretRef); err != nil {
			return nil, err
		}
	} else if git.GitHubUser != "" {
		return nil, validationError("git.githubUser", "requires git.credentialSecretRef")
	}
	if git.SigningKeySecretRef != nil {
		if git.UserEmail == "" {
			return nil, validationError("git.userEmail", "is required when a signing key is configured")
		}
		if git.SigningKeySecretRef.PrivateKey == nil || git.SigningKeySecretRef.PublicKey == nil {
			return nil, validationError("git.signingKeySecretRef", "private and public key references are required")
		}
		if err := validateSecretReference(namespace, "git.signingKeySecretRef.privateKey", *git.SigningKeySecretRef.PrivateKey); err != nil {
			return nil, err
		}
		if err := validateSecretReference(namespace, "git.signingKeySecretRef.publicKey", *git.SigningKeySecretRef.PublicKey); err != nil {
			return nil, err
		}
	}

	files := []FileTarget{{
		Scope:                      FileScopeWorkstation,
		Path:                       gitConfigPath,
		Mode:                       WriteModeManagedBlock,
		Format:                     FileFormatText,
		Content:                    renderGitConfigBlock(git),
		DiscoverGitSafeDirectories: true,
		Apply:                      workstationFileApplyPolicy(),
	}}

	if git.CredentialSecretRef != nil {
		files = append(files, FileTarget{
			Scope:   FileScopeWorkstation,
			Path:    githubConfigPath,
			Mode:    WriteModeReplace,
			Format:  FileFormatYAML,
			Content: "version: 1\n",
			Apply:   workstationFileApplyPolicy(),
		})
		userTokenPath := "/github.com/users/" + git.GitHubUser + "/oauth_token"
		skeleton, err := json.Marshal(map[string]any{
			"github.com": map[string]any{
				"git_protocol": "https",
				"user":         git.GitHubUser,
				"oauth_token":  "",
				"users": map[string]any{
					git.GitHubUser: map[string]any{"oauth_token": ""},
				},
			},
		})
		if err != nil {
			return nil, fmt.Errorf("render GitHub credential skeleton: %w", err)
		}
		files = append(files, FileTarget{
			Scope:      FileScopeWorkstation,
			Path:       githubHostsPath,
			Mode:       WriteModeReplace,
			Format:     FileFormatYAML,
			Content:    string(skeleton),
			OwnedPaths: []string{"/github.com/oauth_token", userTokenPath},
			Values: []FileValueSource{
				{
					Path:      "/github.com/oauth_token",
					ValueFrom: *cloneSecretReference(git.CredentialSecretRef),
				},
				{
					Path:      userTokenPath,
					ValueFrom: *cloneSecretReference(git.CredentialSecretRef),
				},
			},
			Apply: workstationFileApplyPolicy(),
		})
	}

	if git.SigningKeySecretRef != nil {
		privateKey := *cloneSecretReference(git.SigningKeySecretRef.PrivateKey)
		publicKey := *cloneSecretReference(git.SigningKeySecretRef.PublicKey)
		files = append(files,
			FileTarget{
				Scope:  FileScopeWorkstation,
				Path:   gitPrivateKeyPath,
				Mode:   WriteModeReplace,
				Format: FileFormatText,
				SecretContent: &SecretContentSource{
					ValueFrom: privateKey,
					Transform: SecretTransformOpenSSHPrivateKey,
				},
				Apply: workstationFileApplyPolicy(),
			},
			FileTarget{
				Scope:  FileScopeWorkstation,
				Path:   gitPublicKeyPath,
				Mode:   WriteModeReplace,
				Format: FileFormatText,
				SecretContent: &SecretContentSource{
					ValueFrom: publicKey,
					Transform: SecretTransformTrimSpace,
					Suffix:    "\n",
				},
				Apply: workstationFileApplyPolicy(),
			},
			FileTarget{
				Scope:  FileScopeWorkstation,
				Path:   gitAllowedUsersPath,
				Mode:   WriteModeReplace,
				Format: FileFormatText,
				SecretContent: &SecretContentSource{
					ValueFrom: publicKey,
					Transform: SecretTransformTrimSpace,
					Prefix:    git.UserEmail + " ",
					Suffix:    "\n",
				},
				Apply: workstationFileApplyPolicy(),
			},
		)
	}
	return files, nil
}

func renderGitConfigBlock(git GitConfiguration) string {
	var sections []string
	var user []string
	if git.UserName != "" {
		user = append(user, "\tname = "+quoteConfigurationValue(git.UserName))
	}
	if git.UserEmail != "" {
		user = append(user, "\temail = "+quoteConfigurationValue(git.UserEmail))
	}
	if git.SigningKeySecretRef != nil {
		user = append(user, "\tsigningkey = "+gitPublicKeyPath)
	}
	if len(user) != 0 {
		sections = append(sections, "[user]\n"+strings.Join(user, "\n"))
	}
	if git.SigningKeySecretRef != nil {
		sections = append(sections,
			"[gpg]\n\tformat = ssh",
			"[gpg \"ssh\"]\n\tallowedSignersFile = "+gitAllowedUsersPath,
			"[commit]\n\tgpgsign = true",
			"[tag]\n\tgpgsign = true",
		)
	}
	if git.CredentialSecretRef != nil {
		sections = append(sections,
			"[credential \"https://github.com\"]\n\thelper =\n\thelper = !GH_CONFIG_DIR=/data/t3-coded/gh gh auth git-credential",
			"[credential \"https://gist.github.com\"]\n\thelper =\n\thelper = !GH_CONFIG_DIR=/data/t3-coded/gh gh auth git-credential",
		)
	}
	sections = append(sections,
		"[init]\n\tdefaultBranch = main",
		"[push]\n\tautoSetupRemote = true",
		"[pull]\n\trebase = true",
	)
	return strings.Join(sections, "\n") + "\n"
}

func validateSingleLine(path, value string, required bool) error {
	if required && value == "" {
		return validationError(path, "is required")
	}
	for _, character := range value {
		if unicode.IsControl(character) {
			return validationError(path, "must not contain control characters")
		}
	}
	return nil
}

func quoteConfigurationValue(value string) string {
	value = strings.ReplaceAll(value, `\`, `\\`)
	value = strings.ReplaceAll(value, `"`, `\"`)
	return `"` + value + `"`
}

func workstationFileApplyPolicy() ApplyPolicy {
	return ApplyPolicy{
		Class:     ChangeClassDisruptive,
		When:      ApplyWhenIdle,
		Mechanism: ReloadNextSession,
	}
}

func AppendGitSafeDirectories(content string, directories []string) string {
	if len(directories) == 0 {
		return content
	}
	directories = append([]string(nil), directories...)
	sort.Strings(directories)
	var result strings.Builder
	result.WriteString(content)
	result.WriteString("[safe]\n")
	for _, directory := range directories {
		result.WriteString("\tdirectory = ")
		result.WriteString(quoteConfigurationValue(directory))
		result.WriteByte('\n')
	}
	return result.String()
}
