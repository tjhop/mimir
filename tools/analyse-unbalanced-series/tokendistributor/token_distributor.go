package tokendistributor

import (
	"container/heap"
	"fmt"
	"math"
	"strings"

	"golang.org/x/exp/slices"
)

const (
	initialInstanceCount = 66
)

type TokenDistributor struct {
	sortedTokens        []Token
	instanceByToken     map[Token]Instance
	replicationStrategy ReplicationStrategy
	tokensPerInstance   int
	tokensByInstance    map[Instance][]Token
	maxTokenValue       Token
	zoneByInstance      map[Instance]Zone
	zones               []Zone
	seedGenerator       SeedGenerator
}

func NewTokenDistributor(tokensPerInstance, zonesCount int, maxTokenValue Token, replicationStrategy ReplicationStrategy, seedGenerator SeedGenerator) *TokenDistributor {
	sortedTokens := make([]Token, 0, initialInstanceCount*tokensPerInstance)
	instanceByToken := make(map[Token]Instance, initialInstanceCount*tokensPerInstance)
	tokensByInstance := make(map[Instance][]Token, initialInstanceCount)
	zoneByInstance := make(map[Instance]Zone, initialInstanceCount)

	tokenDistributor := &TokenDistributor{
		sortedTokens:        sortedTokens,
		instanceByToken:     instanceByToken,
		zoneByInstance:      zoneByInstance,
		zones:               generateZones(zonesCount),
		replicationStrategy: replicationStrategy,
		tokensByInstance:    tokensByInstance,
		maxTokenValue:       maxTokenValue,
		tokensPerInstance:   tokensPerInstance,
		seedGenerator:       seedGenerator,
	}
	return tokenDistributor
}

// newTokenDistributorFromInitializedInstances supposes that the ring already contains some instances with tokensPerInstance tokens each
func newTokenDistributorFromInitializedInstances(sortedTokens []Token, instanceByToken map[Token]Instance, zonesCount int, zoneByInstance map[Instance]Zone, tokensPerInstance int, replicationStrategy ReplicationStrategy) *TokenDistributor {
	tokensByInstance := make(map[Instance][]Token, len(sortedTokens)/tokensPerInstance)
	for token, instance := range instanceByToken {
		tokens, ok := tokensByInstance[instance]
		if !ok {
			tokens = make([]Token, 0, tokensPerInstance)
		}
		tokens = append(tokens, token)
		tokensByInstance[instance] = tokens
	}
	tokens := make([]Token, 0, initialInstanceCount*tokensPerInstance)
	for _, token := range sortedTokens {
		tokens = append(tokens, token)
	}
	return &TokenDistributor{
		sortedTokens:        tokens,
		instanceByToken:     instanceByToken,
		zoneByInstance:      zoneByInstance,
		zones:               generateZones(zonesCount),
		replicationStrategy: replicationStrategy,
		tokensPerInstance:   tokensPerInstance,
		tokensByInstance:    tokensByInstance,
		maxTokenValue:       maxTokenValue,
		seedGenerator:       nil,
	}
}

func generateZones(zonesCount int) []Zone {
	if zonesCount == 1 {
		return []Zone{SingleZone}
	}
	zones := make([]Zone, 0, zonesCount)
	for i := 0; i < zonesCount; i++ {
		zones = append(zones, Zone("zone-"+string(rune('a'+i))))
	}
	return zones
}

func (t *TokenDistributor) addTokenToInstance(instance Instance, token Token) {
	tokens, ok := t.tokensByInstance[instance]
	if !ok {
		tokens = make([]Token, 0, t.tokensPerInstance)
	}
	tokens = append(tokens, token)
	t.tokensByInstance[instance] = tokens
}

func (t *TokenDistributor) getOptimalTokenOwnership() float64 {
	maxTokenAsFloat64 := float64(t.maxTokenValue)
	return maxTokenAsFloat64 * float64(t.replicationStrategy.getReplicationFactor()) / float64(len(t.sortedTokens)+t.tokensPerInstance)
}

// calculateReplicatedOwnership calculated the replicated weight of the given token with its given replicaStart.
// The replicated weight is basically the distance between replicaStart's predecessor and the token itself.
func (t *TokenDistributor) calculateReplicatedOwnership(token, replicaStart navigableTokenInterface) float64 {
	return t.calculateDistanceAsFloat64(replicaStart.getPrevious(), token)
}

// calculateReplicatedOwnership calculated the replicated weight of the given token with its given replicaStart.
// The replicated weight is basically the distance between replicaStart's predecessor and the token itself.
func (t *TokenDistributor) calculateDistanceAsFloat64(first, second navigableTokenInterface) float64 {
	return float64(first.getToken().distance(second.getToken(), t.maxTokenValue))
}

func (t *TokenDistributor) createInstanceAndZoneInfos() (map[Instance]*instanceInfo, map[Zone]*zoneInfo) {
	zoneInfoByZone := make(map[Zone]*zoneInfo)
	instanceInfoByInstance := make(map[Instance]*instanceInfo, len(t.tokensByInstance))
	for instance, tokens := range t.tokensByInstance {
		zone, ok := t.zoneByInstance[instance]
		if !ok {
			continue
		}
		zoneInfo, ok := zoneInfoByZone[zone]
		if !ok {
			zoneInfo = newZoneInfo(zone)
		}
		zoneInfoByZone[zone] = zoneInfo
		instanceInfoByInstance[instance] = newInstanceInfo(instance, zoneInfoByZone[zone], len(tokens))
	}
	return instanceInfoByInstance, zoneInfoByZone
}

// createTokenInfoCircularList creates a circularList whose elements are of type tokenInfo.
// The resulting circularList is created starting from the slice of sorted tokens sortedTokens, and represent the actual
// ring. For tokenInfo its replica start (tokenInfo.replicaStart) and expandable (tokenInfo.expandable) are calculated
// and stored
func (t *TokenDistributor) createTokenInfoCircularList(instanceInfoByInstance map[Instance]*instanceInfo, newInstance *instanceInfo) *CircularList[*tokenInfo] {
	circularList := newCircularList[*tokenInfo]()
	for _, token := range t.sortedTokens {
		instanceInfo := instanceInfoByInstance[t.instanceByToken[token]]
		navigableToken := newNavigableTokenInfo(newTokenInfo(instanceInfo, token))
		circularList.insertLast(navigableToken)
	}

	curr := circularList.head
	for _ = range t.sortedTokens {
		t.populateTokenInfo(curr.getData(), newInstance)
		curr = curr.next
	}

	//t.count(&circularList)
	return &circularList
}

// createCandidateTokenInfoCircularList creates a circularList whose elements are of type candidateTokenInfo.
// The resulting circularList is created starting from a circularList of tokenInfo elements in such a way that between
// each 2 tokenInfos belonging to the former, a candidateTokenInfo is created in the middle of the 2 tokenInfos.
func (t *TokenDistributor) createCandidateTokenInfoCircularList(tokenInfoCircularList *CircularList[*tokenInfo], newInstanceInfo *instanceInfo, initialTokenOwnership float64) *CircularList[*candidateTokenInfo] {
	circularList := newCircularList[*candidateTokenInfo]()
	curr := tokenInfoCircularList.head
	for {
		currentTokenInfo := curr.getData()
		candidateToken := t.calculateCandidateToken(currentTokenInfo)
		candidateTokenInfo := newCandidateTokenInfo(newInstanceInfo, candidateToken, currentTokenInfo)
		candidateTokenInfo.setReplicatedOwnership(initialTokenOwnership)
		navigableToken := newNavigableCandidateTokenInfo(candidateTokenInfo)
		circularList.insertLast(navigableToken)
		t.populateCandidateTokenInfo(navigableToken.getData(), newInstanceInfo)

		curr = curr.next
		if curr == tokenInfoCircularList.head {
			break
		}
	}
	return &circularList
}

func (t *TokenDistributor) calculateCandidateToken(tokenInfo *tokenInfo) Token {
	currentToken := tokenInfo.getToken()
	nextToken := tokenInfo.getNext().getToken()
	candidate := currentToken.split(nextToken, t.maxTokenValue)
	for slices.Contains(t.sortedTokens, candidate) {
		candidate = candidate.next(t.maxTokenValue)
	}
	return candidate
}

// calculateReplicaStartAndExpansion crosses the ring backwards starting from the token corresponding to navigableToken,
// and looks for the first token belonging to the same zone. The token preceding the latter is a replica start of
// navigableToken, and is returned as a result of calculateReplicaStartAndExpansion.
// Another result returned by calculateReplicaStartAndExpansion is whether the replica set starting in the first result
// of the function could be expanded by a token belonging to newInstanceZone. Expansion is possible in the following
// cases:
// 1) if the replica set is not full (i.e., if tokens between navigableToken and its replica set do not cover all the
// zones) and the replica start is not in newInstanceZone (e.g., "zone-a"->"zone-c"->"zone-a" is expandable by "zone-b",
// but "zone-b"->"zone-c"->"zone-b" is not expandable by "zone-b" because that expansion would actually give raise to
// 2 replica sets "zone-b"->"zone-c" and "zone-b"->"zone-b", or
// 2) if the replica set is not full and if it contains any token from newInstanceZone in the middle (e.g., replica sets
// "zone-a"->"zone-b"->"zone-a" and "zone-a"->"zone-c"->"zone-a" can be extended by "zone-b", or
// 3) if the replica set is full, but it neither starts nor ends with a token from newInstanceZone (e.g., replica sets
// "zone-a"->"zone-b"->"zone-c" and "zone-c"-"zone-b"-"zone-a" can be extended by "zone-b", but replica sets
// "zone-b"->"zone-a"->"zone-c" and "zone-a"->"zone-c"->"zone-b" cannot be extended by "zone-b"
func (t *TokenDistributor) calculateReplicaStartAndExpansion(navigableToken navigableTokenInterface, newInstance *instanceInfo) (navigableTokenInterface, navigableTokenInterface, bool) {
	var (
		prevZoneInfo      = &LastZoneInfo
		prevInstanceInfo  = &LastInstanceInfo
		replicasCount     = 0
		expandable        = false
		replicationFactor = t.replicationStrategy.getReplicationFactor()
		// replicaStart is the farthest away token before a token from the same zone is found
		replicaStart = navigableToken
		rfm1         = replicaStart
		curr         = navigableToken.getPrevious()
	)

	// we are visiting tokens in "backwards" mode, i.e., by going towards the replica start
	for {
		currInstance := curr.getOwningInstance()
		if !t.isAlreadyVisited(currInstance) {
			// if the current instance (or zone according to the mode) has not been visited
			// we mark it and its zone as visited
			currZone := currInstance.zone
			if currZone.precededBy == nil {
				currZone.precededBy = prevZoneInfo
				prevZoneInfo = currZone
			}
			currInstance.precededBy = prevInstanceInfo
			prevInstanceInfo = currInstance
			replicasCount++
			if replicasCount == replicationFactor {
				// we have obtained the full replica
				break
			}

			rfm1 = replicaStart
			// if we find an instance belonging to the same zone as tokenInfo, we have to break
			if t.isBarrier(navigableToken.getOwningInstance(), currInstance) {
				// inserting a new token from newInstanceZone at this boundary expands the replica set size,
				// but only if the current token is not from that zone
				// e.g., if navigableTokenZone is zone-a, and we are trying to place a token from zone-b (newInstanceZone),
				// a partial replica set "zone-a"->"zone-c"->"zone-a" can be extended by zone-b , obtaining replica set
				// "zone-b"->"zone-a"->"zone-b".
				// On the other hand, a partial replica set "zone-b"->"zone-c"->"zone-b" cannot be extended by "zone-b"
				// because that extension would split the replica in 2 different replicas
				expandable = t.isExpandable(currInstance, newInstance, true) //currZone != newInstance.zone
				break
			}

			// it is possible to expand the replica set size by inserting a token of newInstanceZone at
			// replicaStart if there is another token from that zone in the middle
			if t.isExpandable(currInstance, newInstance, false) {
				expandable = true
			}
		}
		replicaStart = curr
		curr = curr.getPrevious()
	}

	// clean up visited instances and zones
	t.unvisitAll(prevInstanceInfo, prevZoneInfo)
	return replicaStart, rfm1, expandable
}

func (t *TokenDistributor) isBarrier(instance, possibleBarrier *instanceInfo) bool {
	currentInstanceZone := instance.zone
	possibleBarrierZone := possibleBarrier.zone
	if possibleBarrierZone.zone != SingleZone {
		return currentInstanceZone == possibleBarrierZone
	}
	return instance == possibleBarrier
}

func (t *TokenDistributor) isExpandable(currentInstance, newInstance *instanceInfo, differentRequired bool) bool {
	currentInstanceZone := currentInstance.zone
	newInstanceZone := newInstance.zone
	if newInstanceZone.zone != SingleZone {
		if differentRequired {
			return currentInstanceZone != newInstanceZone
		}
		return currentInstanceZone == newInstanceZone
	}
	if differentRequired {
		return currentInstance != newInstance
	}
	return currentInstance == newInstance
}

// populateTokenInfo gets as input a tokenInfo, and it calculates and updates its replica start and expandable information
// in the ring. Moreover, it updates the replicated weight information of both token itself and the instance that
// corresponds to that node.
func (t *TokenDistributor) populateTokenInfo(tokenInfo *tokenInfo, newInstance *instanceInfo) {
	replicaStart, rfm1, expandable := t.calculateReplicaStartAndExpansion(tokenInfo, newInstance)
	tokenInfo.setReplicaStart(replicaStart)
	tokenInfo.setRfm1(rfm1)
	tokenInfo.setExpandable(expandable)
	newOwnership := t.calculateReplicatedOwnership(tokenInfo, replicaStart)
	oldOwnership := tokenInfo.getReplicatedOwnership()
	tokenInfo.setReplicatedOwnership(newOwnership)
	tokenInfo.getOwningInstance().ownership += newOwnership - oldOwnership
}

// populateCandidateTokenInfo gets as input a candidateTokenInfo, it calculates and sets its replica start and expandable
// information starting from the position in the ring that corresponds to candidateTokenInfo's host, i.e., the token after
// which the candidate is being added to the ring
func (t *TokenDistributor) populateCandidateTokenInfo(candidateTokenInfo *candidateTokenInfo, newInstance *instanceInfo) {
	replicaStart, rfm1, expandable := t.calculateReplicaStartAndExpansion(candidateTokenInfo, newInstance)
	candidateTokenInfo.setReplicaStart(replicaStart)
	candidateTokenInfo.setRfm1(rfm1)
	candidateTokenInfo.setExpandable(expandable)
}

func (t *TokenDistributor) evaluateImprovement(candidate *candidateTokenInfo, optimalTokenOwnership, multiplier float64) float64 {
	newInstance := candidate.getOwningInstance()
	host := candidate.host
	next := host.getNext()
	improvement := 0.0

	// We first calculate the replication weight of candidate
	oldOwnership := candidate.getReplicatedOwnership()
	newOwnership := t.calculateReplicatedOwnership(candidate, candidate.getReplicaStart().(navigableTokenInterface))
	stdev := calculateImprovement(optimalTokenOwnership, newOwnership, oldOwnership, newInstance.tokenCount)
	improvement += stdev
	newInstance.adjustedOwnership = newInstance.ownership + newOwnership - oldOwnership
	stats := strings.Builder{}
	stats.WriteString(fmt.Sprintf("Adding candidate token %d (delta: %.3f, impr: %.5f) affects the following tokens:\n",
		candidate.getToken(), newOwnership/optimalTokenOwnership, stdev))

	// We now calculate potential changes in the replicated weight of the tokenInfo that succeeds the host (i.e., next).
	// This tokenInfo (next) is of interest because candidate has been placed between host and next.

	// On the other hand. the replicated weight of next can change

	// The replicated weight of host and tokenInfos preceding it cannot be changed because they precede candidate,
	// and the replicated weight is the distance between the (further) replica start of a token and the token itself.
	// Therefore, we cross the ring backwards starting from the tokenInfo that succeeds the host (i.e., next) and calculate
	// potential improvements of all the tokenInfos that could be potentially affected by candidate's insertion. This
	// basically means that we are looking for all the tokenInfos that replicate the candidate.
	// We keep track of visited zones, so that we don't count the same zone more than once. Once the count of visited
	// zones is equal to replicationStrategy.getReplicationFactor(), we can stop the visit. The reason for that is that
	// no other token that precedes the token of the last visited zone could contain a replica of candidate because
	// between that token (included) and candidate (excluded) there would be all available zones, and therefore candidate
	// would have already been replicated replicationStrategy.getReplicationFactor() times. In other words, the replica
	// start of that token would succeed candidate on the ring.
	//

	newInstanceZone := newInstance.zone
	//newInstance.precededBy = newInstance
	prevInstance := newInstance
	replicasCount := 0
	for currToken := next; replicasCount < t.replicationStrategy.getReplicationFactor(); currToken = currToken.getNext() {
		currInstance := currToken.getOwningInstance()
		currZone := currInstance.zone
		// if this instance has already been visited, we go on
		if t.isAlreadyVisited(currInstance) {
			continue
		}
		// otherwise we mark these zone and instance as visited
		currZone.precededBy = currZone
		replicasCount++

		// If this is the firs time we encounter an instance, we initialize its adjustedOwnership and mark it as seen.
		// The adjustedOwnership of each node will be used for the final evaluation of weight, together with all
		// potential improvements of all visited tokens
		if currInstance.precededBy == nil {
			currInstance.adjustedOwnership = currInstance.ownership
			currInstance.precededBy = prevInstance
			prevInstance = currInstance
		}

		//newReplicaStart := currToken.getReplicaStart()
		barrier := currToken.getReplicaStart().(navigableTokenInterface).getPrevious()
		if currToken.isExpandable() {
			if currToken.getReplicaStart() == next {
				// If currToken is expandable (wrt. candidate's zone), then it is possible to extend its replica set by adding
				// a token from  candidate's zone as a direct predecessor of currToken replica start. Token candidate is being
				// evaluated as the direct predecessor of token next, and therefore only that case would be valid here. In this
				// case candidate would become the new replica start of currToken.
				//newReplicaStart = candidate
				barrier = host
			} else {
				continue
			}
		} else if t.isBarrier(currInstance, newInstance) {
			if currToken.getReplicatedOwnership() > t.calculateDistanceAsFloat64(candidate, currToken) {
				// Barrier of a token is the first token belonging to the same zone of the former by crossing the ring
				// backwards, i.e., it is the direct predecessor of replica start of that token.
				// If currToken and candidate belong to the same zone, the only change that could happen here is if
				// candidate becomes a new barrier of currToken, and this is possible only if the current barrier of
				// currentToken precedes candidate.
				// Proof: if the current barrier of currentToken succeeds candidate, candidate would be at least the
				// second occurrence of the zone of currentToken, which contradicts the definition of term barrier.
				// In order to check whether the current barrier precedes candidate, we could compare the current
				// replicated weight of currToken (i.e., the range of tokens from its barrier to curentToken itself)
				// with the range of tokens from candidate to currentToken itself, and if the former is greater, we
				// can state that the barrier precedes candidate. In that case, candidate becomes the new barrier of
				// currentToken, and replica start of the latter, being it the direct successor of a barrier, would
				// be node next.
				barrier = candidate
			} else {
				continue
			}
		} else if t.isBarrier(currInstance, host.getOwningInstance()) {
			barrier = candidate
		} /*else if currZone == next.getOwningInstance().zone && currToken.getReplicatedOwnership() > t.calculateDistanceAsFloat64(candidate, currToken) {
			// If the barrier of the current token
			// Does this really make sense?
			barrier = next
		}*/

		oldOwnership = currToken.getReplicatedOwnership()
		newOwnership = t.calculateDistanceAsFloat64(barrier, currToken)
		stdev = calculateImprovement(optimalTokenOwnership, newOwnership, oldOwnership, currInstance.tokenCount)
		improvement += stdev
		currInstance.adjustedOwnership += newOwnership - oldOwnership
		if newOwnership != oldOwnership {
			stats.WriteString(fmt.Sprintf("\ttoken %d [%s-%s] (old %.3f -> new %.3f, impr: %.5f)\n",
				currToken.getToken(), currInstance.instanceId, currZone.zone, oldOwnership/optimalTokenOwnership, newOwnership/optimalTokenOwnership, stdev))
		}
	}

	stats.WriteString(fmt.Sprintf("Candidate token %d affects the following instances:\n", candidate.getToken()))
	// Go through all visited instances and zones, add recorder adjustedOwnership and clear visited instances and zones
	currInstance := prevInstance
	for currInstance != nil {
		newOwnership = prevInstance.adjustedOwnership
		oldOwnership = prevInstance.ownership
		tokenCount := float64(prevInstance.tokenCount)
		diff := sq(oldOwnership/tokenCount-optimalTokenOwnership) - sq(newOwnership/tokenCount-optimalTokenOwnership)
		if currInstance == newInstance {
			diff *= multiplier
		}
		improvement += diff
		prevInstance = currInstance.precededBy
		currInstance.precededBy = nil
		currInstance.zone.precededBy = nil
		currInstance.adjustedOwnership = 0.0
		if newOwnership != oldOwnership {
			oldOptimalTokenOwnership := tokenCount * optimalTokenOwnership
			newOptimalTokenOwnership := oldOptimalTokenOwnership
			if currInstance == newInstance {
				newOptimalTokenOwnership += optimalTokenOwnership
			}
			stats.WriteString(fmt.Sprintf("\t[%s-%s] (old %.3f -> new %.3f, impr: %.5f)\n",
				currInstance.instanceId, currInstance.zone.zone, oldOwnership/oldOptimalTokenOwnership, newOwnership/newOptimalTokenOwnership, diff))
		}
		currInstance = prevInstance
	}
	stats.WriteString(fmt.Sprintf("Evaluation of candidate %d [%s-%s] gives rise to the following improvement: %.5f\n",
		candidate.getToken(), newInstance.instanceId, newInstanceZone.zone, improvement))
	//fmt.Print(stats.String())

	return improvement
}

func (t *TokenDistributor) getStatistics(tokenInfoCircularList *CircularList[*tokenInfo], optimalTokenOwnership float64) Statistics {
	statisticType := make(map[string]StatisticType, 2)
	token := StatisticType{}
	instance := StatisticType{}
	registeredOwnersByInstance := make(map[Instance]float64, len(t.tokensByInstance))

	totalTokens := float64(t.maxTokenValue) * float64(t.replicationStrategy.getReplicationFactor())
	head := tokenInfoCircularList.head
	curr := head
	token.minDistanceFromOptimalTokenOwnership = math.MaxFloat64
	token.maxDistanceFromOptimalTokenOwnership = math.SmallestNonzeroFloat64
	token.minOwnership = math.MaxFloat64
	token.maxOwnership = math.SmallestNonzeroFloat64
	token.optimalTokenOwnership = optimalTokenOwnership
	token.standardDeviation = 0.0
	token.sum = 0.0
	instance.minDistanceFromOptimalTokenOwnership = math.MaxFloat64
	instance.maxDistanceFromOptimalTokenOwnership = math.SmallestNonzeroFloat64
	instance.minOwnership = math.MaxFloat64
	instance.maxOwnership = math.SmallestNonzeroFloat64
	instance.optimalTokenOwnership = float64(t.tokensPerInstance) * optimalTokenOwnership
	instance.standardDeviation = 0.0
	instance.sum = 0.0
	for {
		dist := curr.getData().getReplicatedOwnership() / optimalTokenOwnership
		currTokensPercentage := curr.getData().getReplicatedOwnership() * 100.00 / totalTokens
		if token.minDistanceFromOptimalTokenOwnership > dist {
			token.minDistanceFromOptimalTokenOwnership = dist
		}
		if token.maxDistanceFromOptimalTokenOwnership < dist {
			token.maxDistanceFromOptimalTokenOwnership = dist
		}
		if token.minOwnership > currTokensPercentage {
			token.minOwnership = currTokensPercentage
		}
		if token.maxOwnership < currTokensPercentage {
			token.maxOwnership = currTokensPercentage
		}
		token.standardDeviation += +sq(dist - 1.0)
		token.sum += curr.getData().getReplicatedOwnership()

		dist = curr.getData().getOwningInstance().ownership / (optimalTokenOwnership * float64(t.tokensPerInstance))
		currTokensPercentage = curr.getData().getOwningInstance().ownership * 100.00 / totalTokens
		if instance.minDistanceFromOptimalTokenOwnership > dist {
			instance.minDistanceFromOptimalTokenOwnership = dist
		}
		if instance.maxDistanceFromOptimalTokenOwnership < dist {
			instance.maxDistanceFromOptimalTokenOwnership = dist
		}
		if instance.minOwnership > currTokensPercentage {
			instance.minOwnership = currTokensPercentage
		}
		if instance.maxOwnership < currTokensPercentage {
			instance.maxOwnership = currTokensPercentage
		}
		instance.standardDeviation += +sq(dist - 1.0)
		instance.sum += curr.getData().getOwningInstance().ownership

		ownership, ok := registeredOwnersByInstance[curr.getData().getOwningInstance().instanceId]
		if !ok {
			ownership = 0
		}
		ownership += curr.getData().getOwningInstance().ownership
		registeredOwnersByInstance[curr.getData().getOwningInstance().instanceId] = ownership

		curr = curr.next
		if curr == head {
			break
		}
	}
	statisticType["token"] = token
	statisticType["instance"] = instance

	return Statistics{
		CombinedStatistics:                 statisticType,
		RegisteredTokenOwnershipByInstance: registeredOwnersByInstance,
	}
}

func (t *TokenDistributor) count(tokenInfoCircularList *CircularList[*tokenInfo]) {
	head := tokenInfoCircularList.head
	curr := head
	counts := make(map[Instance]int)
	s := 0.0
	for {
		count, ok := counts[curr.getData().getOwningInstance().instanceId]
		if !ok {
			count = 0
			s += curr.getData().getOwningInstance().ownership
		}
		count++
		counts[curr.getData().getOwningInstance().instanceId] = count
		curr = curr.next
		if curr == head {
			break
		}
	}
	fmt.Println(s, counts)
}

func sq(value float64) float64 {
	return value * value
}

func calculateImprovement(optimalTokenOwnership, newOwnership, oldOwnership float64, tokenCount int) float64 {
	return (sq(oldOwnership-optimalTokenOwnership) - sq(newOwnership-optimalTokenOwnership)) / sq(float64(tokenCount))
}

func (t *TokenDistributor) createPriorityQueue(candidateTokenInfoCircularList *CircularList[*candidateTokenInfo], optimalTokenOwnership float64, tokenCount int) *PriorityQueue[*candidateTokenInfo] {
	pq := newPriorityQueue[*candidateTokenInfo](len(t.sortedTokens), true)
	head := candidateTokenInfoCircularList.head
	curr := head
	for {
		improvement := t.evaluateImprovement(curr.getData(), optimalTokenOwnership, 1/float64(tokenCount))
		weightedNavigableToken := newWeightedNavigableToken[*candidateTokenInfo](improvement)
		setNavigableToken(weightedNavigableToken, curr)
		pq.Add(weightedNavigableToken)
		curr = curr.next
		if curr == head {
			break
		}
	}
	heap.Init(pq)
	return pq
}

func (t *TokenDistributor) insertBefore(element, before *navigableToken[*tokenInfo], list *CircularList[*tokenInfo]) {
	element.insertBefore(before)
	if list.head == before {
		list.head = element
	}
}

// isAlreadyVisited returns true if the instance passed as parameter has already been visited.
// An instance is considered as visited if zone-awareness is disabled, and a token belonging to the passed instance has
// been considered, or if zone-awareness is enabled, and a token belonging to any instance of the zone of the passed
// instance has been considered.
func (t *TokenDistributor) isAlreadyVisited(instance *instanceInfo) bool {
	zone := instance.zone
	if zone.zone == SingleZone {
		// if it is a single zone mode, then we check whether the given instance has already been visited
		return instance.precededBy != nil
	}
	// if it is a mulit-zone mode, we check whether the given zone has already been visited
	return zone.precededBy != nil
}

// unvisitAll clears all the details about visited instances and zones in the chain of visited instances and zones.
func (t *TokenDistributor) unvisitAll(instance *instanceInfo, zone *zoneInfo) {
	prevZone := zone
	prevInstance := instance
	currZone := prevZone
	currInstance := prevInstance
	for {
		if currZone != &LastZoneInfo {
			prevZone = currZone.precededBy
			currZone.precededBy = nil
			currZone = prevZone
		}
		if currInstance != &LastInstanceInfo {
			prevInstance = currInstance.precededBy
			currInstance.precededBy = nil
			currInstance = prevInstance
		}
		if currZone == &LastZoneInfo && currInstance == &LastInstanceInfo {
			break
		}
	}
}

// verifyReplicaStart is used just to double-check that the replica start has been correctly computed
// should not be used in production
func (t *TokenDistributor) verifyReplicaStart(candidate *candidateTokenInfo) {
	_, found := t.instanceByToken[candidate.getToken()]
	// When candidates are evaluated, they don't belong to the token slice, and they have no instance assigned to them.
	// If it is the case, we partially assign their possible instance to the token being evaluated for the check, and
	// then remove it after the check.
	t.instanceByToken[candidate.getToken()] = candidate.getOwningInstance().instanceId
	rs, err := t.replicationStrategy.getReplicaStart(candidate.getToken(), t.sortedTokens, t.instanceByToken)
	if err != nil {
		fmt.Printf("\t\tIt was not possible to find the replica start of token %d (%s) by using the replication strategy\n", candidate.getToken(), candidate.getOwningInstance())
	}
	if rs != candidate.getReplicaStart().getToken() {
		fmt.Printf("\t\tReplica start of %d is %d according to the list, and %d according to the replication srategy\n", candidate.getToken(), candidate.getReplicaStart().getToken(), rs)
	}
	if !found {
		delete(t.instanceByToken, candidate.getToken())
	}
}

// addCandidateToTokenInfoCircularList gets as parameters a navigableToken[*candidateTokenInfo], representing a candidate
// token, and a CircularList[*tokenInfo], representing a circular list of tokens belonging to the ring, and adds the former
// to the latter. In order to do that, a tokenInfo is created, inserted in the list, and all its relevant information
// such as replica start, extensibility and replicated token ownership are calculated. Moreover, these details are updated
// for all the tokens affected by this insertion. Similarly, all the candidates following the candidate passed as parameter
// that might be affected by this insertion are also modified in the list of candidates
func (t *TokenDistributor) addCandidateToTokenInfoCircularList(navigableCandidate *navigableToken[*candidateTokenInfo], tokenInfoCircularList *CircularList[*tokenInfo]) {
	candidate := navigableCandidate.getData()
	host := candidate.host
	next := host.getNext()
	change := 0.0
	newInstance := candidate.getOwningInstance()
	//newInstanceZone := newInstance.zone
	t.verifyReplicaStart(candidate)
	newTokenInfo := newTokenInfo(newInstance, candidate.getToken())
	newTokenInfo.setReplicaStart(candidate.getReplicaStart())
	newReplicatedOwnership := t.calculateReplicatedOwnership(newTokenInfo, newTokenInfo.getReplicaStart().(navigableTokenInterface))
	//oldTokenOwnership := newTokenInfo.getReplicatedOwnership()
	newTokenInfo.setReplicatedOwnership(newReplicatedOwnership)
	//oldInst := newInstance.ownership
	newInstance.ownership += newReplicatedOwnership
	change += newReplicatedOwnership
	//fmt.Printf("\tToken %d got a new replicated ownership %.2f->%.2f and its instance %s %.2f->%.2f\n", newTokenInfo.getToken(), oldTokenOwnership, newTokenInfo.getReplicatedOwnership(), newInstance.instanceId, oldInst, newInstance.ownership)
	navigableToken := newNavigableTokenInfo(newTokenInfo)
	t.insertBefore(navigableToken, next.getNavigableToken(), tokenInfoCircularList)

	replicasCount := 0
	prevZone := &LastZoneInfo
	prevInstance := &LastInstanceInfo
	for currToken := next; replicasCount < t.replicationStrategy.getReplicationFactor(); currToken = currToken.getNext() {
		currInstance := currToken.getOwningInstance()
		currZone := currInstance.zone
		// if the current instance has already been visited, we continue
		if t.isAlreadyVisited(currInstance) {
			continue
		}
		// otherwise we mark these instance and zone as visited and increase the counter of visited zones
		if currZone.precededBy == nil {
			currZone.precededBy = prevZone
			prevZone = currZone
		}
		currInstance.precededBy = prevInstance
		prevInstance = currInstance
		replicasCount++

		var barrier navigableTokenInterface
		if currToken.isExpandable() {
			if currToken.getReplicaStart() == next {
				// If currToken is expandable (wrt. candidate's zone), then it is possible to extend its replica set by adding
				// a token from  candidate's zone as a direct predecessor of currToken replica start, without changing the
				// replicated token ownership. Token candidate is being evaluated as the direct predecessor of token next,
				// and therefore the only valid case here would be if the replica start of currToken is next, and its barrier
				// is host. In this case candidate would become the new replica start of currToken.
				currToken.setReplicaStart(newTokenInfo)
				// since the barrier didn't change, currToken remains expandable
				currToken.setExpandable(true)
				barrier = host
				//fmt.Printf("\tToken %d got a new replica start %d and new barrier %d\n", currToken.getToken(), currToken.getReplicaStart().getToken(), barrier.getToken())
			} else {
				continue
			}
		} else if t.isBarrier(currInstance, newInstance) {
			if currToken.getReplicatedOwnership() > t.calculateDistanceAsFloat64(candidate, currToken) {
				// Barrier of a token is the first token belonging to the same zone of the former by crossing the ring
				// backwards, i.e., it is the direct predecessor of replica start of that token.
				// If currToken and candidate belong to the same zone, the only change that could happen here is if
				// candidate becomes a new barrier of currToken, and this is possible only if the current barrier of
				// currentToken precedes candidate.
				// Proof: if the current barrier of currentToken succeeds candidate, candidate would be at least the
				// second occurrence of the zone of currentToken, which contradicts the definition of term barrier.
				// In order to check whether the current barrier precedes candidate, we could compare the current
				// replicated weight of currToken (i.e., the range of tokens from its barrier to currentToken itself)
				// with the range of tokens from candidate to currentToken itself, and if the former is greater, we
				// can state that the barrier precedes candidate. In that case, candidate becomes the new barrier of
				// currentToken, and replica start of the latter, being it the direct successor of a barrier, would
				// be node next.
				currToken.setReplicaStart(next)
				// currToken and candidate belong to the same zone, and candidate is a new barrier of currToken. Hence,
				// currToken cannot be expandable.
				currToken.setExpandable(false)
				barrier = candidate
				//fmt.Printf("\tToken %d got a new replica start %d and new barrier %d\n", currToken.getToken(), currToken.getReplicaStart().getToken(), barrier.getToken())
			} else {
				continue
			}
		} else if currToken.getReplicatedOwnership() > t.calculateDistanceAsFloat64(candidate, currToken) {
			if currToken.getReplicaStart() == next.(navigableTokenInterface) {
				barrier = candidate
				currToken.setReplicaStart(next)
				currToken.setExpandable(false)
				//fmt.Printf("\tToken %d got a new replica start %d and new barrier %d\n", currToken.getToken(), currToken.getReplicaStart().getToken(), barrier.getToken())
			} else {
				barrier = currToken.getReplicaStart().(navigableTokenInterface)
				currToken.setReplicaStart(barrier.getNext())
				currToken.setExpandable(true)
				//fmt.Printf("\tToken %d got a new replica start %d and new barrier %d\n", currToken.getToken(), currToken.getReplicaStart().getToken(), barrier.getToken())
			}
		}
		oldOwnership := currToken.getReplicatedOwnership()
		newOwnership := t.calculateDistanceAsFloat64(barrier, currToken)
		currToken.setReplicatedOwnership(newOwnership)
		//old := currInstance.ownership
		change += newOwnership - oldOwnership
		currInstance.ownership += newOwnership - oldOwnership
		if newOwnership != oldOwnership {
			//fmt.Printf("\tToken %d got a new replicated ownership %.2f->%.2f and its instance %s %.2f->%.2f\n", currToken.getToken(), oldOwnership, currToken.getReplicatedOwnership(), currInstance.instanceId, old, currInstance.ownership)
		}
	}

	// we cancel all the "visited" info
	t.unvisitAll(prevInstance, prevZone)

	// we need to refresh the candidates that might have been affected by these changes
	//nextCandidate := navigableCandidate.next.getData()
	nextNavigableCandidate := navigableCandidate
	for {
		nextNavigableCandidate = nextNavigableCandidate.next
		nextCandidate := nextNavigableCandidate.getData()
		if t.calculateReplicatedOwnership(nextCandidate, nextCandidate.getReplicaStart().(navigableTokenInterface)) > t.calculateDistanceAsFloat64(candidate, nextCandidate) {
			// if current replica start of the next candidate is more distant from the next candidate than candidate,
			// then candidate is the new barrier of the next candidate
			//fmt.Printf("\tToken %d affected candidate %d, its replica start became %d\n", candidate.getToken(), nextCandidate.getToken(), next.getToken())
			nextCandidate.setReplicaStart(next)
		} else {
			break
		}
	}

	//fmt.Printf("Token %d has been added to the list, instance %s %.3f->%.3f (%.3f)\n", candidate.getToken(), newInstance, oldInst, newInstance.ownership, change)
}

func (t *TokenDistributor) addNewInstanceAndToken(instance *instanceInfo, token Token) {
	t.sortedTokens = append(t.sortedTokens, token)
	slices.Sort(t.sortedTokens)
	t.instanceByToken[token] = instance.instanceId
	t.zoneByInstance[instance.instanceId] = instance.zone.zone
	t.addTokenToInstance(instance.instanceId, token)
}

func (t *TokenDistributor) addTokensFromSeed(instance Instance, zone Zone) {
	seed, ok := t.seedGenerator.getNextSeed(zone)
	if ok {
		for _, token := range seed {
			t.sortedTokens = append(t.sortedTokens, token)
			t.addTokenToInstance(instance, token)
			t.instanceByToken[token] = instance
		}
		t.zoneByInstance[instance] = zone
		slices.Sort(t.sortedTokens)
	}
}

func printInstanceOwnership(instanceInfoByInstance map[Instance]*instanceInfo) {
	zoneOwnership := make(map[Zone]float64)
	sum := 0.0
	//fmt.Println(len(instanceInfoByInstance))
	for _, instanceInfo := range instanceInfoByInstance {
		zone := instanceInfo.zone.zone
		ownership, ok := zoneOwnership[zone]
		if !ok {
			ownership = 0
		}
		ownership += instanceInfo.ownership
		zoneOwnership[zone] = ownership
		if strings.Contains(string(instanceInfo.instanceId), "A-") {
			sum += instanceInfo.ownership
		}
		//fmt.Printf("[%s-%.2f] ", instance, instanceInfoByInstance[instance].ownership)
	}
	//fmt.Println()
}

func (t *TokenDistributor) AddInstance(instance Instance, zone Zone) (*CircularList[*tokenInfo], *CircularList[*candidateTokenInfo], Statistics) {
	t.replicationStrategy.addInstance(instance, zone)
	if t.seedGenerator != nil && t.seedGenerator.hasNextSeed(zone) {
		t.addTokensFromSeed(instance, zone)
		return nil, nil, Statistics{}
	}

	instanceInfoByInstance, zoneInfoByZone := t.createInstanceAndZoneInfos()
	newInstanceZone, ok := zoneInfoByZone[zone]
	if !ok {
		newInstanceZone = newZoneInfo(zone)
		zoneInfoByZone[zone] = newInstanceZone
	}
	newInstanceInfo := newInstanceInfo(instance, newInstanceZone, t.tokensPerInstance)
	instanceInfoByInstance[instance] = newInstanceInfo
	tokenInfoCircularList := t.createTokenInfoCircularList(instanceInfoByInstance, newInstanceInfo)
	//fmt.Printf("\t\t\t%s", instanceInfoByInstance)
	optimalTokenOwnership := t.getOptimalTokenOwnership()
	candidateTokenInfoCircularList := t.createCandidateTokenInfoCircularList(tokenInfoCircularList, newInstanceInfo, optimalTokenOwnership)

	pq := t.createPriorityQueue(candidateTokenInfoCircularList, optimalTokenOwnership, t.tokensPerInstance)
	//fmt.Printf("Priority queue: %s\n", pq)
	bestToken := heap.Pop(pq).(*WeightedNavigableToken[*candidateTokenInfo])
	//fmt.Printf("Token %s [%.5f] is the best candidate\n", bestToken.navigableToken, bestToken.weight)

	addedTokens := 0
	for {
		bestCandidate := bestToken.navigableToken
		printInstanceOwnership(instanceInfoByInstance)
		t.addNewInstanceAndToken(newInstanceInfo, bestToken.navigableToken.getData().getToken())
		t.addCandidateToTokenInfoCircularList(bestCandidate, tokenInfoCircularList)
		candidateTokenInfoCircularList.remove(bestToken.navigableToken)
		addedTokens++
		if addedTokens == t.tokensPerInstance {
			break
		}

		for {
			//fmt.Printf("Iteration %d, priority queue: %s\n", i, pq)
			bestToken = heap.Pop(pq).(*WeightedNavigableToken[*candidateTokenInfo])
			//fmt.Printf("\ttokenInfoCircularList: %s\n", tokenInfoCircularList)
			candidate := getNavigableToken[*candidateTokenInfo](bestToken).getData()
			// at this point i new tokens have already been added to the new instance,
			// so we are looking for the (i + 1)st candidate. Therefore, the multiplier
			// tho use in evaluateImprovement is (i + 1) / t.tokensPerInstance
			newImprovement := t.evaluateImprovement(candidate, optimalTokenOwnership, float64(addedTokens+1)/float64(t.tokensPerInstance))
			if pq.Len() == 0 {
				break
			}
			nextBestToken := pq.Peek()
			//fmt.Printf("Understanding whether token %s [%.5f] is still the best. Its new weight is %.5f, and the current elemement with the max weight in pq is %s [%.5f]\n", bestToken.navigableToken, bestToken.weight, newImprovement, nextBestToken.navigableToken, nextBestToken.weight)
			if newImprovement >= nextBestToken.weight {
				//fmt.Printf("Token %s [%.5f] is the best candidate\n", bestToken.navigableToken, bestToken.weight)
				break
			}
			navigableToken := getNavigableToken[*candidateTokenInfo](bestToken)
			bestToken = newWeightedNavigableToken[*candidateTokenInfo](newImprovement)
			setNavigableToken[*candidateTokenInfo](bestToken, navigableToken)
			heap.Push(pq, bestToken)
			//fmt.Printf("Token %s was not the best, so we put it back on pq and retry\n", bestToken.navigableToken)
		}
	}

	stat := t.getStatistics(tokenInfoCircularList, optimalTokenOwnership)
	printInstanceOwnership(instanceInfoByInstance)
	//t.count(tokenInfoCircularList)
	//fmt.Println("-----------------------------------------------------------")
	if stat.CombinedStatistics["instance"].sum != float64(t.maxTokenValue)*float64(t.tokensPerInstance*t.replicationStrategy.getReplicationFactor()) {
		fmt.Printf("During insertion of instance %s sum of replication ownership was %.2f instead of %.2f\n", instance, stat.CombinedStatistics["instance"].sum, float64(t.maxTokenValue)*float64(t.tokensPerInstance*t.replicationStrategy.getReplicationFactor()))
	}

	return tokenInfoCircularList, candidateTokenInfoCircularList, stat
}
