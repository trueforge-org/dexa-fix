package web

import "github.com/trueforge-org/dexa-fix/pkg/apiclient"

// The views of the settings and estimates pages are built by the backend and sent to the frontend
// as apiclient types; these names keep the page code short.
type (
	integrationView   = apiclient.IntegrationView
	auditLine         = apiclient.AuditLine
	securityView      = apiclient.SecurityView
	tokenView         = apiclient.TokenView
	linkedProvider    = apiclient.LinkedProvider
	twoFactorSetup    = apiclient.TwoFactorSetup
	twoFactorSettings = apiclient.TwoFactorSettings
	passkeysView      = apiclient.PasskeysView
	passkeyItem       = apiclient.PasskeyItem
	slJobView         = apiclient.SLJobView
	slView            = apiclient.SLView
)
