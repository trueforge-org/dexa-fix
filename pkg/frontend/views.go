package frontend

import "github.com/trueforge-org/dexa-fix/pkg/shared/apiclient"

// The views of the settings and estimates pages are built by the backend and sent to the frontend
// as apiclient types; these names keep the page code short.
type (
	integrationView = apiclient.IntegrationView
	securityView    = apiclient.SecurityView
	slView          = apiclient.SLView
)
