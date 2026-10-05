# Azure: roles and Workload Identity

What the operator's Azure identities hold, and how the operator authenticates as them.
`test/deploy/azure_iam_test.go` checks the roles: discovery reads and nothing else, the writer
never deletes, and the reader covers every Resource Manager call discovery makes (it runs the
provider against the in-repo fake and maps each request to its action).

```
                     service account token (audience api://AzureADTokenExchange)
 operator pod ──────────────────────────────────────────────────► Microsoft Entra ID
   │  one projected token, exchanged per identity                     │ checks the federated identity
   │                                                                  │ credential of the client ID
   ▼                                                                  ▼
 own identity (providers.azure.workloadIdentity.clientId)     accounts[].azure.clientID / writeClientID
   │ reads subscriptions whose entry names no identity           │ each with its own role assignments
   ▼                                                              ▼
 Azure Resource Manager ◄──────────── bearer token of the identity the target uses
```

Azure has no AssumeRole. Every identity the operator uses, its own and the ones a `NetworkScope`
names per subscription, is a user-assigned managed identity or an app registration with a
**federated identity credential** that trusts the operator's Kubernetes service account. The
operator exchanges the same service account token for a token of whichever identity a target
uses. So the choice between the two layouts below is a matter of role assignments:

- **One identity for everything** (the default): the operator's own identity holds the reader
  role (and the writer role for writes) in every subscription, or on a management group above
  them. Account entries need no `azure` member. Simple, but discovery then carries the write
  permissions.
- **Identities per account**: `accounts[].azure.clientID` (reader) and `writeClientID` (writer),
  one pair per subscription or per landing zone, each with its role assignments where it reads
  or writes. Reads and writes stay apart, and a subscription's owner decides what the operator
  may do there. The operator's own identity then needs no role at all (it may even be left out:
  `providers.azure.workloadIdentity.clientId` empty). An identity may serve many subscriptions;
  one identity has at most 20 federated identity credentials, one per cluster and service account.
- **Another tenant**: managed identities are single-tenant. Use a multitenant app registration
  in the operator's tenant with the federated credential, provision it (admin consent) in the
  other tenant, assign the roles there, and name both in the account:
  `azure: {clientID: <app>, tenantID: <other tenant>}`.

Tokens are requested from `accounts[].azure.tenantID`, or from the operator's own tenant
(`providers.azure.tenantId`, `AZURE_TENANT_ID`). A token Entra refuses fails the target with
reason `SyncFailed` and an error naming the client ID, the issuer and subject the federated
credential must name, and Entra's `AADSTS` code; the operator never falls back to its own
identity.

Change events (Event Grid to a Storage queue the operator polls) are optional and have a page
of their own: [events.md](events.md), with the Bicep templates in [`events/`](events).

## Roles

`az role definition create` takes the files as they are, after the placeholder in
`AssignableScopes` is replaced with the subscriptions or management groups the role may be
assigned in.

| File | Actions | Assign to |
|---|---|---|
| [`reader-role.json`](reader-role.json) | `Microsoft.Network/virtualNetworks/read` (List All, List per resource group, and Get of the virtual network before a `ResourceImport`), `.../virtualNetworks/usages/read` (addresses used per subnet), `.../virtualNetworks/subnets/read` | the read identity, on each subscription, or only on the resource groups of a scope that names them (`spec.azure.resourceGroups`) |
| [`writer-role.json`](writer-role.json) (#53) | `Microsoft.Network/virtualNetworks/subnets/write` (create a subnet) and `/read`, `Microsoft.Network/locations/operations/read` (poll the create), `Microsoft.Resources/tags/read` and `/write` (ownership tags on the virtual network through the Tags API, without `virtualNetworks/write`), `virtualNetworks/read` | the write identity, where claims create subnets and imports tag; imports alone need only Tag Contributor |

Never granted: any `*/delete`, `Microsoft.Network/virtualNetworks/write` (which could change the
address space or peerings), `Microsoft.Authorization/*`. Discovery lists subnets inline with
their virtual networks, so whether the reader needs `subnets/read`, and whether a subnet with a
route table or network security group also needs their `join/action`, is for the run against
real subscriptions (#56) to confirm. `Microsoft.Resources/subscriptions/resourceGroups/read`
is not needed: discovery never reads a resource group itself.

```sh
SUB=00000000-0000-0000-0000-000000000000
sed "s#/subscriptions/00000000-0000-0000-0000-000000000000#/subscriptions/$SUB#" \
  deploy/azure/reader-role.json > /tmp/reader-role.json
az role definition create --role-definition @/tmp/reader-role.json
az role assignment create --assignee "$READER_CLIENT_ID" --role "Subnet operator reader" \
  --scope "/subscriptions/$SUB"
```

## Workload Identity

The federated identity credential names the cluster's service account issuer (its OIDC issuer
URL: `az aks show --query oidcIssuerProfile.issuerUrl` on AKS, the EKS OIDC provider URL, the
GKE issuer `https://container.googleapis.com/v1/projects/<project>/locations/<location>/clusters/<cluster>`),
the operator's service account as subject, and the audience `api://AzureADTokenExchange`.

```sh
ISSUER=https://oidc.example/cluster           # the cluster's service account issuer
SUBJECT=system:serviceaccount:subnet-operator-system:subnet-operator
RG=rg-identities

# The operator's own identity, and a reader and writer for a landing zone.
for name in subnet-operator subnet-reader-lz1 subnet-writer-lz1; do
  az identity create --resource-group "$RG" --name "$name"
  az identity federated-credential create --resource-group "$RG" --identity-name "$name" \
    --name "$(echo "$ISSUER" | md5sum | cut -c1-12)-subnet-operator" \
    --issuer "$ISSUER" --subject "$SUBJECT" --audiences api://AzureADTokenExchange
done
az identity show --resource-group "$RG" --name subnet-reader-lz1 --query clientId -o tsv
```

The same in Terraform (`azurerm`):

```hcl
resource "azurerm_user_assigned_identity" "subnet_reader" {
  name                = "subnet-reader-lz1"
  resource_group_name = "rg-identities"
  location            = "westeurope"
}

resource "azurerm_federated_identity_credential" "subnet_reader" {
  name                = "subnet-operator"
  resource_group_name = azurerm_user_assigned_identity.subnet_reader.resource_group_name
  parent_id           = azurerm_user_assigned_identity.subnet_reader.id
  issuer              = var.cluster_oidc_issuer_url
  subject             = "system:serviceaccount:subnet-operator-system:subnet-operator"
  audience            = ["api://AzureADTokenExchange"]
}

resource "azurerm_role_definition" "subnet_reader" {
  name        = "Subnet operator reader"
  scope       = "/subscriptions/${var.subscription_id}"
  description = jsondecode(file("deploy/azure/reader-role.json")).Description
  permissions {
    actions = jsondecode(file("deploy/azure/reader-role.json")).Actions
  }
  assignable_scopes = ["/subscriptions/${var.subscription_id}"]
}

resource "azurerm_role_assignment" "subnet_reader" {
  scope              = "/subscriptions/${var.subscription_id}"
  role_definition_id = azurerm_role_definition.subnet_reader.role_definition_resource_id
  principal_id       = azurerm_user_assigned_identity.subnet_reader.principal_id
}
```

Then, in the chart, `providers.azure.workloadIdentity.enabled: true` with the operator's own
client ID in `workloadIdentity.clientId`: on AKS with the workload identity add-on the webhook
projects the token (`workloadIdentity.webhook: true`, the default); anywhere else set
`webhook: false` and `providers.azure.tenantId`, and the chart projects it. And in the scope:

```yaml
spec:
  provider: Azure
  accounts:
    - id: 5b3c2a10-0000-4000-8000-00000000a2e1
      azure:
        clientID: <client ID of subnet-reader-lz1>
        writeClientID: <client ID of subnet-writer-lz1>
```

A new federated credential takes a few minutes to propagate; until then Entra answers
`AADSTS70021` and the target is retried on the next sync.
