package cache

import (
	"fmt"
	"github.com/GrappigPanda/Olivia/bloomfilter"
	"github.com/GrappigPanda/Olivia/bloomfilter/search"
	"github.com/GrappigPanda/Olivia/config"
	"github.com/GrappigPanda/Olivia/dht"
	"github.com/GrappigPanda/Olivia/network/message_handler"
	binheap "github.com/GrappigPanda/Olivia/shared"
	"log"
	"strings"
	"sync"
	"time"
)

type Cache struct {
	PeerList          *dht.PeerList
	bloomfilterSearch *bfsearch.Search
	MessageBus        *message_handler.MessageHandler
	cache             *map[string]string
	binHeap           *binheap.Heap
	bloomFilter       bloomfilter.BloomFilter
	sync.Mutex
}

// NewCache creates a new cache and internal ReadCache.
func NewCache(mh *message_handler.MessageHandler, config *config.Cfg) *Cache {
	cacheMap := make(map[string]string)
	cache := &Cache{
		PeerList:          nil,
		bloomfilterSearch: nil,
		MessageBus:        mh,
		cache:             &cacheMap,
		binHeap:           binheap.NewHeapReallocate(100),
		bloomFilter:       bloomfilter.NewByFailRate(1000, 0.01),
	}

	if config != nil {
		cache.PeerList = dht.NewPeerList(mh, *config)
		for index, peerIP := range config.RemotePeers {
			peer := dht.NewPeerByIP(peerIP, mh, *config)
			cache.PeerList.Peers[index] = peer
			(*cache.PeerList.PeerMap)[peerIP] = true
		}

		if !config.IsTesting && !config.BaseNode {
			err := cache.PeerList.ConnectAllPeers()
			for err != nil {
				log.Println("Sleeping for 60 seconds and attempting to reconnect")
				time.Sleep(time.Second * 2)
				err = cache.PeerList.ConnectAllPeers()
			}
		}
	}

	cache.Heartbeat()

	return cache
}

// Get handles retrieving a value by its key from the internal cache. It reads
// from the ReadCache which is for copy-on-write optimizations so that
// reading doesn't lock the cache.
func (c *Cache) Get(key string) (string, error) {
	if value, ok := (*c.cache)[key]; !ok {
		if c.PeerList != nil && len(c.PeerList.Peers) > 0 {
			return c.getFromRemotePeers(key)
		}
	} else {
		return value, nil
	}
	return "", fmt.Errorf("Key not found in cache")
}

func (c *Cache) getFromRemotePeers(key string) (string, error) {
	responseChannel := make(chan string)

	if c.bloomfilterSearch == nil {
		return "", fmt.Errorf("bloomfilterSearch is uninitialized")
	}
	indices := c.bloomFilter.HashKey([]byte(key))
	foundPeers := c.bloomfilterSearch.GetFromIndices(indices)

	for _, peer := range foundPeers {
		// TODO(ian): Pull out the dht.Timeout and dht.Disconnected to an `isConnectable` function.
		if peer == nil || peer.Status == dht.Timeout || peer.Status == dht.Disconnected {
			continue
		}

		peer.SendRequest(
			fmt.Sprintf("GET %s", key),
			responseChannel,
			c.MessageBus,
		)

		value := <-responseChannel
		if value != "" {
			splitString := strings.Split(value, " ")
			splitString = strings.Split(splitString[1], ":")
			if len(splitString) > 1 {
				return fmt.Sprintf("%s:%s", key, splitString[1]), nil
			} else {
				return fmt.Sprintf("%s:%s", key, ""), nil
			}
		}
	}
	return "", fmt.Errorf("Key not found in cache")
}

// copyCache handles creating a copy of the cache
func (c *Cache) copyCache() {
	c.Lock()
	for k, v := range *c.cache {
		(*c.cache)[k] = v
	}
	c.Unlock()
}

// Set handles adding a key/value pair to the cache and updating the internal
// ReadCache.
func (c *Cache) Set(key string, value string) error {
	c.Lock()
	(*c.cache)[key] = value
	c.bloomFilter.AddKey([]byte(key))
	c.Unlock()

	c.copyCache()

	return nil
}

// SetExpiration handles setting a key with an expiration time.
func (c *Cache) SetExpiration(key string, value string, timeout int) error {
	err := c.Set(key, value)
	if err != nil {
		return err
	}

	duration := time.Duration(timeout) * time.Second
	c.binHeap.Insert(binheap.NewNode(key, time.Now().UTC().Add(duration)))

	c.copyCache()
	return err
}

// EvictExpiredKeys handles
func (c *Cache) EvictExpiredkeys(expirationDate time.Time) {
	keysToExpire := make([]string, len(c.binHeap.Tree))

	i := 0

	c.Lock()
	for {
		node, err := c.binHeap.Peek(i)
		if err != nil {
			break
		}

		if expirationDate.Sub(node.Timeout) < 0 {
			break
		} else {
			keysToExpire = append(keysToExpire, node.Key)
		}

		i++
	}

	for _, key := range keysToExpire {
		c.expireKey(key)
	}
	c.Unlock()
}

func (c *Cache) expireKey(key string) {
	delete(*c.cache, key)
	// TODO(ian): We need to also remove the the key from the binary heap.
}

func (c *Cache) DisconnectPeer(peerIPPort string) string {
	outString := "Peer not found in peer list."
	for _, peer := range c.PeerList.Peers {
		if peer.IPPort != peerIPPort {
			continue
		}

		if peer != nil && peer.Status == dht.Connected {
			peer.Disconnect()
			outString = "Peer has been disconnected."
		}

		// TODO(ian): Connect a backup node after one node has forced itself to be evicted.
	}

	return outString
}

func (c *Cache) AddPeer(peerIPPort string) {
	c.PeerList.AddPeer(peerIPPort)

	if c.bloomfilterSearch == nil {
		c.bloomfilterSearch = bfsearch.NewSearch(*c.PeerList)
	} else {
		c.bloomfilterSearch.Recalculate(*c.PeerList)
	}
}

func (c *Cache) ListPeers(requestHash string) string {
	count := 0
	outString := fmt.Sprintf("%s:FULFILLED ", requestHash)

	for _, peer := range c.PeerList.Peers {
		if peer == nil {
			continue
		}

		if count == 0 {
			outString = fmt.Sprintf(
				"%s%s",
				outString,
				peer.IPPort,
			)

		} else {
			outString = fmt.Sprintf(
				"%s,%s",
				outString,
				peer.IPPort,
			)

		}
	}

	for _, peer := range c.PeerList.BackupPeers {
		if peer == nil {
			continue
		}

		outString = fmt.Sprintf(
			"%s,%s",
			outString,
			peer.IPPort,
		)
	}

	return fmt.Sprintf(
		"%s\n",
		outString,
	)
}

func (c *Cache) GetBloomFilter() bloomfilter.BloomFilter {
	return c.bloomFilter
}
