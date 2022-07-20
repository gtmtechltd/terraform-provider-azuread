package serviceprincipals

import (
	"context"
	"fmt"
	"net/http"
	"reflect"
	"time"

	"github.com/hashicorp/terraform-plugin-sdk/v2/helper/resource"
	"github.com/hashicorp/terraform-plugin-sdk/v2/helper/schema"
	"github.com/hashicorp/terraform-provider-azuread/internal/helpers"
	"github.com/hashicorp/terraform-provider-azuread/internal/utils"
	"github.com/manicminer/hamilton/msgraph"
	"github.com/manicminer/hamilton/odata"
)

func servicePrincipalAppRoleChanged(existing msgraph.AppRole, new msgraph.AppRole) bool {
	if !reflect.DeepEqual(existing.AllowedMemberTypes, new.AllowedMemberTypes) {
		return true
	}
	if !reflect.DeepEqual(existing.Description, new.Description) {
		return true
	}
	if !reflect.DeepEqual(existing.DisplayName, new.DisplayName) {
		return true
	}

	// The following order is important; we must check for nil, and we consider nil and "" to be equivalent Values
	if reflect.DeepEqual(existing.Value, new.Value) {
		return false
	}
	if existing.Value == nil && new.Value != nil && *new.Value == "" {
		return false
	}
	if existing.Value != nil && *existing.Value == "" && new.Value == nil {
		return false
	}
	return true
}

func servicePrincipalGetAppRoles(ctx context.Context, client *msgraph.ServicePrincipalsClient, servicePrincipal *msgraph.ServicePrincipal) ([]msgraph.AppRole, []msgraph.AppRole, []msgraph.AppRole, error) {
	if servicePrincipal.ID == nil {
		return nil, nil, nil, fmt.Errorf("cannot use ServicePrincipal model with nil ID")
	}

	sp, status, err := client.Get(ctx, *servicePrincipal.ID, odata.Query{})
	if err != nil {
		if status == http.StatusNotFound {
			return nil, nil, nil, fmt.Errorf("servicePrincipal with ID %q was not found", *servicePrincipal.ID)
		}

		return nil, nil, nil, fmt.Errorf("retrieving ServicePrincipal with object ID %q: %+v", *servicePrincipal.ID, err)
	}

	var spAppRoles []msgraph.AppRole
	var applicationAppRoles []msgraph.AppRole
	var allAppRoles []msgraph.AppRole
	if sp.AppRoles != nil {
		spAppRoles = *helpers.AppRolesFilterByOrigin(sp.AppRoles, "ServicePrincipal")
		applicationAppRoles = *helpers.AppRolesFilterByOrigin(sp.AppRoles, "Application")
	} else {
		spAppRoles = []msgraph.AppRole{}
		applicationAppRoles = []msgraph.AppRole{}
	}
	allAppRoles = append(applicationAppRoles, spAppRoles...)
	return applicationAppRoles, spAppRoles, allAppRoles, nil
}

func servicePrincipalDisableUnwantedAppRoles(ctx context.Context, client *msgraph.ServicePrincipalsClient, servicePrincipal *msgraph.ServicePrincipal, desiredAppRoles *[]msgraph.AppRole) error {
	if servicePrincipal.ID == nil {
		return fmt.Errorf("cannot use ServicePrincipal model with nil ID")
	}

	if desiredAppRoles == nil {
		desiredAppRoles = &[]msgraph.AppRole{}
	}

	existingApplicationAppRoles, existingSpAppRoles, _, err := servicePrincipalGetAppRoles(ctx, client, servicePrincipal)

	if err != nil {
		return err
	}

	// Shortcut: don't update if no changes to be made
	if reflect.DeepEqual(existingSpAppRoles, *desiredAppRoles) {
		return nil
	}

	// Identify any roles to be changed
	var disable bool
	for _, new := range *desiredAppRoles {
		if new.ID == nil || *new.ID == "" {
			return fmt.Errorf("new role provided with nil or empty ID")
		}
		for i, existing := range existingSpAppRoles {
			if existing.ID != nil && *existing.ID == *new.ID {
				if existing.IsEnabled != nil && *existing.IsEnabled && servicePrincipalAppRoleChanged(existing, new) {
					*existingSpAppRoles[i].IsEnabled = false
					disable = true
				}
				break
			}
		}
	}

	// Identify any roles to be removed
	for i, existing := range existingSpAppRoles {
		found := false
		for _, new := range *desiredAppRoles {
			if existing.ID != nil && *new.ID == *existing.ID {
				found = true
				break
			}
		}
		if !found {
			*existingSpAppRoles[i].IsEnabled = false
			disable = true
		}
	}

	if disable {
		patchAppRoles := append(existingApplicationAppRoles, existingSpAppRoles...)
		// Disable any changed or removed roles
		properties := msgraph.ServicePrincipal{
			DirectoryObject: msgraph.DirectoryObject{
				ID: servicePrincipal.ID,
			},
			AppRoles: &patchAppRoles,
		}
		if _, err := client.Update(ctx, properties); err != nil {
			return fmt.Errorf("disabling AppRoles for ServicePrincipal %q: %+v", *servicePrincipal.ID, err)
		}

		// Wait for servicePrincipal manifest to reflect the disabled roles
		deadline, ok := ctx.Deadline()
		if !ok {
			return fmt.Errorf("context has no deadline")
		}
		timeout := time.Until(deadline)
		_, err = (&resource.StateChangeConf{
			Pending:    []string{"Waiting"},
			Target:     []string{"Disabled"},
			Timeout:    timeout,
			MinTimeout: 1 * time.Second,
			Refresh: func() (interface{}, string, error) {
				sp, _, err := client.Get(ctx, *servicePrincipal.ID, odata.Query{})
				if err != nil {
					return nil, "Error", fmt.Errorf("retrieving ServicePrincipal %q: %+v", *servicePrincipal.ID, err)
				}
				if sp == nil || sp.AppRoles == nil {
					return nil, "Error", fmt.Errorf("reading AppRoles for ServicePrincipal %q: %+v", *servicePrincipal.ID, err)
				}
				actualRoles := *sp.AppRoles
				for _, expectedRole := range existingSpAppRoles {
					if expectedRole.IsEnabled != nil && !*expectedRole.IsEnabled {
						for _, actualRole := range actualRoles {
							if expectedRole.ID != nil && actualRole.ID != nil && *expectedRole.ID == *actualRole.ID {
								if actualRole.IsEnabled != nil && *actualRole.IsEnabled {
									return actualRoles, "Waiting", nil
								}
								break
							}
						}
					}
				}
				return actualRoles, "Disabled", nil
			},
		}).WaitForStateContext(ctx)
		if err != nil {
			return fmt.Errorf("waiting for AppRoles to be disabled for ServicePrincipal %q: %+v", *servicePrincipal.ID, err)
		}
	}
	return nil
}

func expandServicePrincipalAppRoles(input []interface{}) *[]msgraph.AppRole {
	result := make([]msgraph.AppRole, 0)
	if len(input) == 0 {
		return &result
	}
	for _, appRoleRaw := range input {
		if appRoleRaw == nil {
			continue
		}
		appRole := appRoleRaw.(map[string]interface{})
		var allowedMemberTypes []msgraph.AppRoleAllowedMemberType
		for _, allowedMemberType := range appRole["allowed_member_types"].(*schema.Set).List() {
			allowedMemberTypes = append(allowedMemberTypes, allowedMemberType.(string))
		}
		newAppRole := msgraph.AppRole{
			ID:                 utils.String(appRole["id"].(string)),
			AllowedMemberTypes: &allowedMemberTypes,
			Description:        utils.String(appRole["description"].(string)),
			DisplayName:        utils.String(appRole["display_name"].(string)),
			IsEnabled:          utils.Bool(appRole["enabled"].(bool)),
		}
		if v, ok := appRole["value"]; ok {
			newAppRole.Value = utils.String(v.(string))
		}
		result = append(result, newAppRole)
	}
	return &result
}

func expandSamlSingleSignOn(in []interface{}) *msgraph.SamlSingleSignOnSettings {
	result := msgraph.SamlSingleSignOnSettings{}
	if len(in) == 0 || in[0] == nil {
		return &result
	}

	samlSingleSignOnSettings := in[0].(map[string]interface{})

	result.RelayState = utils.String(samlSingleSignOnSettings["relay_state"].(string))

	return &result
}

func flattenSamlSingleSignOn(in *msgraph.SamlSingleSignOnSettings) []map[string]interface{} {
	if in == nil {
		return []map[string]interface{}{}
	}

	relayState := ""
	if in.RelayState != nil {
		relayState = *in.RelayState
	}

	return []map[string]interface{}{{
		"relay_state": relayState,
	}}
}

// func flattenServicePrincipalAppRoles(in *[]msgraph.AppRole) []map[string]interface{} {
// 	return helpers.ServicePrincipalFlattenAppRoles(in)
// }

func filterServicePrincipalAppRolesByOrigin(in *[]msgraph.AppRole, origin string) []map[string]interface{} {
	return helpers.ServicePrincipalFilterAppRolesByOrigin(in, origin)
}
