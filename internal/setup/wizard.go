package setup

import (
	"fmt"
	"net/url"
	"os"
	"strings"

	"github.com/charmbracelet/huh"
	"github.com/charmbracelet/lipgloss"
	"github.com/lhpalacio/redash-wire/internal/config"
	"github.com/lhpalacio/redash-wire/internal/redash"
)

type Result struct {
	ProfileName string
	RedashURL   string
	APIKey      string
	Username    string
	Password    string
	HasPostgres bool
	HasMySQL    bool
	ReadOnly    bool
}

// RunWizard collects connection details interactively. initialProfile pre-fills
// the profile name (from the -profile flag) so the config it writes matches the
// profile the caller will load.
func RunWizard(initialProfile string) (*Result, error) {
	var (
		profileName = initialProfile
		redashURL   string
		apiKey      string
		readOnly    bool
	)

	form := huh.NewForm(
		huh.NewGroup(
			huh.NewInput().
				Title("Profile name").
				Description("A name for this configuration profile").
				Placeholder("default").
				Value(&profileName),

			huh.NewInput().
				Title("Redash URL").
				Description("The base URL of your Redash instance").
				Placeholder("https://redash.example.com").
				Value(&redashURL).
				Validate(ValidateURL),

			huh.NewInput().
				Title("API Key").
				Description("Your Redash user API key").
				EchoMode(huh.EchoModePassword).
				Value(&apiKey).
				Validate(validateNotEmpty("API Key")),

			huh.NewConfirm().
				Title("Read-only mode").
				Description("Refuse INSERT, UPDATE, DELETE and schema changes, so only reads reach Redash. Good for a profile an AI agent will use.").
				Affirmative("Yes").
				Negative("No").
				Value(&readOnly),
		),
	)

	if err := form.Run(); err != nil {
		return nil, err
	}

	if profileName == "" {
		profileName = "default"
	}

	redashURL = strings.TrimRight(redashURL, "/")

	hasPg, hasMySQL, err := runConnectionTest(redashURL, apiKey)
	if err != nil {
		return nil, err
	}
	// No compatible sources detected (or the user chose to save despite a
	// failed connection): enable both listeners so the written config passes
	// the at-least-one-listener validation and starts a working server.
	if !hasPg && !hasMySQL {
		hasPg, hasMySQL = true, true
	}

	return &Result{
		ProfileName: profileName,
		RedashURL:   redashURL,
		APIKey:      apiKey,
		Username:    config.DefaultUsername,
		Password:    config.DefaultPassword,
		HasPostgres: hasPg,
		HasMySQL:    hasMySQL,
		ReadOnly:    readOnly,
	}, nil
}

func runConnectionTest(redashURL, apiKey string) (hasPostgres, hasMySQL bool, err error) {
	dim := lipgloss.NewStyle().Foreground(lipgloss.Color("8"))
	success := lipgloss.NewStyle().Foreground(lipgloss.Color("2"))
	fail := lipgloss.NewStyle().Foreground(lipgloss.Color("1"))

	fmt.Fprintf(os.Stderr, "  %s\n", dim.Render("Testing connection..."))

	session, sources, connErr := ValidateConnection(redashURL, apiKey)
	if connErr != nil {
		fmt.Fprintf(os.Stderr, "  %s %s\n\n", fail.Render("x"), connErr.Error())

		var saveAnyway bool
		confirm := huh.NewForm(
			huh.NewGroup(
				huh.NewConfirm().
					Title("Save configuration anyway?").
					Value(&saveAnyway),
			),
		)
		if confirmErr := confirm.Run(); confirmErr != nil {
			return false, false, confirmErr
		}
		if !saveAnyway {
			return false, false, fmt.Errorf("setup cancelled")
		}
		return false, false, nil
	}

	userInfo := ""
	if session.User.Name != "" {
		userInfo = fmt.Sprintf(" as %s (%s)", session.User.Name, session.User.Email)
	}
	version := ""
	if session.ClientConfig.Version != "" {
		version = fmt.Sprintf(", Redash %s", session.ClientConfig.Version)
	}

	fmt.Fprintf(os.Stderr, "  %s Connected%s%s\n", success.Render("✓"), userInfo, version)

	for _, ds := range sources {
		if redash.IsPostgresCompatible(ds.Type) {
			hasPostgres = true
		}
		if redash.IsMySQLCompatible(ds.Type) {
			hasMySQL = true
		}
	}

	return hasPostgres, hasMySQL, nil
}

// ValidateURL accepts an absolute http or https URL. init applies it too, so a
// bare host is a usage mistake there rather than a failed connection.
func ValidateURL(s string) error {
	if s == "" {
		return fmt.Errorf("Redash URL is required")
	}
	u, err := url.Parse(s)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return fmt.Errorf("must be a valid URL (e.g. https://redash.example.com)")
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("URL scheme must be http or https")
	}
	return nil
}

func validateNotEmpty(field string) func(string) error {
	return func(s string) error {
		if strings.TrimSpace(s) == "" {
			return fmt.Errorf("%s is required", field)
		}
		return nil
	}
}
