package mainbackend

import "github.com/trueforge-org/dexafix/pkg/shared/apiclient"

// The views of the settings and estimates pages are built here and sent to the frontend as
// apiclient types; these names keep the code short.
type (
	integrationView = apiclient.IntegrationView
	auditLine       = apiclient.AuditLine
	securityView    = apiclient.SecurityView
	twoFactorSetup  = apiclient.TwoFactorSetup
	passkeysView    = apiclient.PasskeysView
	slJobView       = apiclient.SLJobView
	slView          = apiclient.SLView
	tokenView       = apiclient.TokenView
	linkedProvider  = apiclient.LinkedProvider
	passkeyItem     = apiclient.PasskeyItem
)
