// Lets one Event Grid system topic add messages to the operator's queue, and nothing else.
// A module of topic.bicep, deployed to the queue's resource group.

targetScope = 'resourceGroup'

param storageAccountName string
param queueName string

@description('Object (principal) ID of the system topic\'s managed identity.')
param principalId string

var queueDataMessageSender = 'c6a89b2d-59bc-44d0-9896-0f6e12d7b80a'

resource account 'Microsoft.Storage/storageAccounts@2023-05-01' existing = {
  name: storageAccountName
}

resource queues 'Microsoft.Storage/storageAccounts/queueServices@2023-05-01' existing = {
  parent: account
  name: 'default'
}

resource queue 'Microsoft.Storage/storageAccounts/queueServices/queues@2023-05-01' existing = {
  parent: queues
  name: queueName
}

resource topicWrites 'Microsoft.Authorization/roleAssignments@2022-04-01' = {
  name: guid(queue.id, principalId, queueDataMessageSender)
  scope: queue
  properties: {
    principalId: principalId
    principalType: 'ServicePrincipal'
    roleDefinitionId: subscriptionResourceId('Microsoft.Authorization/roleDefinitions', queueDataMessageSender)
  }
}
