// The Storage queue the subnet operator polls for change events, and the operator's right to
// read it. Deploy once, to a resource group of the subscription that holds shared tooling:
//
//   az deployment group create --resource-group rg-subnet-operator \
//     --template-file deploy/azure/events/queue.bicep \
//     --parameters storageAccountName=<globally unique> operatorPrincipalId=<object ID>
//
// Then deploy topic.bicep to every subscription the operator discovers. See ../events.md.

targetScope = 'resourceGroup'

@description('Name of the storage account: 3 to 24 lowercase letters and digits, globally unique.')
@minLength(3)
@maxLength(24)
param storageAccountName string

@description('Name of the queue.')
param queueName string = 'subnet-operator-events'

@description('Object (principal) ID of the operator\'s own identity: the managed identity or the service principal of the app registration its service account is federated with.')
param operatorPrincipalId string

param location string = resourceGroup().location

// Peek, receive and delete messages: the only data actions the operator uses. It cannot add
// messages, read other queues' or touch the account.
var queueDataMessageProcessor = '8a0f0c08-91a1-4084-bc3d-661d67233fed'

resource account 'Microsoft.Storage/storageAccounts@2023-05-01' = {
  name: storageAccountName
  location: location
  kind: 'StorageV2'
  sku: {
    name: 'Standard_LRS'
  }
  properties: {
    // No account keys and no SAS: Event Grid writes and the operator reads as Microsoft Entra
    // identities, each with one data role on the queue.
    allowSharedKeyAccess: false
    allowBlobPublicAccess: false
    minimumTlsVersion: 'TLS1_2'
    supportsHttpsTrafficOnly: true
  }
}

resource queues 'Microsoft.Storage/storageAccounts/queueServices@2023-05-01' = {
  parent: account
  name: 'default'
}

resource queue 'Microsoft.Storage/storageAccounts/queueServices/queues@2023-05-01' = {
  parent: queues
  name: queueName
}

resource operatorReads 'Microsoft.Authorization/roleAssignments@2022-04-01' = {
  name: guid(queue.id, operatorPrincipalId, queueDataMessageProcessor)
  scope: queue
  properties: {
    principalId: operatorPrincipalId
    principalType: 'ServicePrincipal'
    roleDefinitionId: subscriptionResourceId('Microsoft.Authorization/roleDefinitions', queueDataMessageProcessor)
  }
}

@description('Set as the chart value providers.azure.events.queueUrl.')
output queueUrl string = '${account.properties.primaryEndpoints.queue}${queueName}'

@description('Pass to topic.bicep as storageAccountId.')
output storageAccountId string = account.id
