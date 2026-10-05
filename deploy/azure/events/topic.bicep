// The Event Grid system topic of one subscription and the event subscription that delivers its
// virtual network events to the operator's queue (queue.bicep). Deploy to a resource group of
// every subscription the operator discovers; a subscription has at most one system topic for
// its resource events, so reuse an existing one by its name.
//
//   az deployment group create --subscription <watched subscription> --resource-group rg-subnet-operator \
//     --template-file deploy/azure/events/topic.bicep --parameters storageAccountId=<queue.bicep output>
//
// In a busy subscription whose NetworkScope names spec.azure.resourceGroups, pass the same
// groups as resourceGroupNames, so that the virtual networks of the others do not reach the
// queue:
//
//     --parameters storageAccountId=<...> resourceGroupNames='["rg-network-prod","rg-hub"]'
//
// See ../events.md.

targetScope = 'resourceGroup'

@description('Resource ID of the storage account that holds the queue: the storageAccountId output of queue.bicep.')
param storageAccountId string

@description('Name of the queue.')
param queueName string = 'subnet-operator-events'

@description('Name of the subscription\'s system topic.')
param systemTopicName string = 'subnet-operator-events'

@description('Name of the event subscription on the system topic. Change it to deploy a second one, for more than 24 resource groups.')
param eventSubscriptionName string = 'subnet-operator-virtual-networks'

@description('Resource groups whose virtual network events are delivered; empty for every resource group of the subscription. Name the groups of the NetworkScope\'s spec.azure.resourceGroups.')
@maxLength(24)
param resourceGroupNames array = []

var account = split(storageAccountId, '/')

// The subject of a resource event is the resource's ID, so the events of one resource group
// begin with its ID. The slash at the end keeps rg-net from also matching rg-network.
var resourceGroupPrefixes = [for name in resourceGroupNames: '${subscription().id}/resourceGroups/${name}/']

// Virtual networks and everything below them (subnets, peerings, and the tags that hold the
// ownership of subnets). Event Grid's advanced filters compare strings without regard to case,
// which matters here: Azure does not spell resourceGroups, or a group's name, the same way in
// every event.
var resourceTypeFilter = {
  operatorType: 'StringContains'
  key: 'subject'
  values: [
    '/providers/Microsoft.Network/virtualNetworks/'
  ]
}

// Every advanced filter must match, and one matches when any of its values does: a virtual
// network, in one of the resource groups. An event subscription takes 25 filter values in all,
// one of which is the resource type's, hence at most 24 groups; more need a second deployment
// with another eventSubscriptionName.
var resourceGroupFilter = {
  operatorType: 'StringBeginsWith'
  key: 'subject'
  values: resourceGroupPrefixes
}

// The topic's own identity delivers the events, so the storage account needs no account key.
resource topic 'Microsoft.EventGrid/systemTopics@2022-06-15' = {
  name: systemTopicName
  location: 'global'
  identity: {
    type: 'SystemAssigned'
  }
  properties: {
    source: subscription().id
    topicType: 'Microsoft.Resources.Subscriptions'
  }
}

// The queue usually lives in another subscription, which a role assignment has to be deployed
// to; the event subscription is refused until the identity may write to the queue.
module topicWrites 'sender-role.bicep' = {
  name: 'subnet-operator-events-sender-${uniqueString(subscription().id)}'
  scope: resourceGroup(account[2], account[4])
  params: {
    storageAccountName: account[8]
    queueName: queueName
    principalId: topic.identity.principalId
  }
}

resource virtualNetworks 'Microsoft.EventGrid/systemTopics/eventSubscriptions@2022-06-15' = {
  parent: topic
  name: eventSubscriptionName
  dependsOn: [
    topicWrites
  ]
  properties: {
    eventDeliverySchema: 'EventGridSchema'
    filter: {
      // Changes that happened. Failures and cancellations changed nothing, and the action
      // events include every NIC that joins a subnet.
      includedEventTypes: [
        'Microsoft.Resources.ResourceWriteSuccess'
        'Microsoft.Resources.ResourceDeleteSuccess'
      ]
      // In every resource group, unless resourceGroupNames names some.
      advancedFilters: empty(resourceGroupNames)
        ? [
            resourceTypeFilter
          ]
        : [
            resourceTypeFilter
            resourceGroupFilter
          ]
    }
    deliveryWithResourceIdentity: {
      identity: {
        type: 'SystemAssigned'
      }
      destination: {
        endpointType: 'StorageQueue'
        properties: {
          resourceId: storageAccountId
          queueName: queueName
          // An operator that was down for a day resyncs everything anyway.
          queueMessageTimeToLiveInSeconds: 86400
        }
      }
    }
    retryPolicy: {
      maxDeliveryAttempts: 30
      eventTimeToLiveInMinutes: 1440
    }
  }
}
