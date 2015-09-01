package control

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/intelsdi-x/pulse/control/plugin/cpolicy"
	"github.com/intelsdi-x/pulse/core"
	"github.com/intelsdi-x/pulse/core/cdata"
	"github.com/intelsdi-x/pulse/core/ctypes"
)

var (
	errMetricNotFound   = errors.New("metric not found")
	errNegativeSubCount = errors.New("subscription count cannot be < 0")
)

func errorMetricNotFound(ns []string) error {
	return errors.New(fmt.Sprintf("Metric not found: %s", strings.Join(ns, "/")))
}

type metricCatalogItem struct {
	namespace string
	versions  map[int]core.Metric
}

func (m *metricCatalogItem) Namespace() string {
	return m.namespace
}

func (m *metricCatalogItem) Versions() map[int]core.Metric {
	return m.versions
}

type metricType struct {
	Plugin             *loadedPlugin
	namespace          []string
	version            int
	lastAdvertisedTime time.Time
	subscriptions      int
	policy             processesConfigData
	config             *cdata.ConfigDataNode
	data               interface{}
	source             string
	timestamp          time.Time
}

type processesConfigData interface {
	Process(map[string]ctypes.ConfigValue) (*map[string]ctypes.ConfigValue, *cpolicy.ProcessingErrors)
}

func newMetricType(ns []string, last time.Time, plugin *loadedPlugin) *metricType {
	return &metricType{
		Plugin: plugin,

		namespace:          ns,
		lastAdvertisedTime: last,
	}
}

func (m *metricType) Key() string {
	return fmt.Sprintf("/%s/%d", strings.Join(m.Namespace(), "/"), m.Version())
}

func (m *metricType) Namespace() []string {
	return m.namespace
}

func (m *metricType) NamespaceAsString() string {
	return "/" + strings.Join(m.Namespace(), "/")
}

func (m *metricType) Data() interface{} {
	return m.data
}

func (m *metricType) LastAdvertisedTime() time.Time {
	return m.lastAdvertisedTime
}

func (m *metricType) Subscribe() {
	m.subscriptions++
}

func (m *metricType) Unsubscribe() error {
	if m.subscriptions == 0 {
		return errNegativeSubCount
	}
	m.subscriptions--
	return nil
}

func (m *metricType) SubscriptionCount() int {
	return m.subscriptions
}

func (m *metricType) Version() int {
	if m.version > 0 {
		return m.version
	}
	if m.Plugin == nil {
		return -1
	}
	return m.Plugin.Version()
}

func (m *metricType) Config() *cdata.ConfigDataNode {
	return m.config
}

func (m *metricType) Source() string {
	return m.source
}

func (m *metricType) Timestamp() time.Time {
	return m.timestamp
}

type metricCatalog struct {
	tree        *MTTrie
	mutex       *sync.Mutex
	keys        []string
	currentIter int
}

func newMetricCatalog() *metricCatalog {
	var k []string
	return &metricCatalog{
		tree:        NewMTTrie(),
		mutex:       &sync.Mutex{},
		currentIter: 0,
		keys:        k,
	}
}

func (mc *metricCatalog) AddLoadedMetricType(lp *loadedPlugin, mt core.Metric) {
	if lp.ConfigPolicyTree == nil {
		panic("NO")
	}

	newMt := metricType{
		Plugin:             lp,
		namespace:          mt.Namespace(),
		version:            mt.Version(),
		lastAdvertisedTime: mt.LastAdvertisedTime(),
		policy:             lp.ConfigPolicyTree.Get(mt.Namespace()),
	}
	mc.Add(&newMt)
}

func (mc *metricCatalog) RmUnloadedPluginMetrics(lp *loadedPlugin) {
	mc.mutex.Lock()
	defer mc.mutex.Unlock()
	mc.tree.DeleteByPlugin(lp)
}

// Add adds a metricType
func (mc *metricCatalog) Add(m *metricType) {
	mc.mutex.Lock()
	defer mc.mutex.Unlock()

	key := getMetricKey(m.Namespace())
	mc.keys = appendIfMissing(mc.keys, key)

	mc.tree.Add(m)
}

// Get retrieves a loadedPlugin given a namespace and version.
// If provided a version of -1 the latest plugin will be returned.
func (mc *metricCatalog) Get(ns []string, version int) (*metricType, error) {
	mc.mutex.Lock()
	defer mc.mutex.Unlock()
	return mc.get(ns, version)
}

// Fetch transactionally retrieves all loadedPlugins
func (mc *metricCatalog) Fetch(ns []string) ([]*metricType, error) {
	mc.mutex.Lock()
	defer mc.mutex.Unlock()

	mtsi, err := mc.tree.Fetch(ns)
	if err != nil {
		return nil, err
	}
	return mtsi, nil
}

func (mc *metricCatalog) Remove(ns []string) {
	mc.mutex.Lock()
	mc.tree.Remove(ns)
	mc.mutex.Unlock()
}

// Item returns the current metricType in the collection.  The method Next()
// provides the  means to move the iterator forward.
func (mc *metricCatalog) Item() (string, []*metricType) {
	key := mc.keys[mc.currentIter-1]
	ns := strings.Split(key, ".")
	mtsi, _ := mc.tree.Get(ns)
	var mts []*metricType
	for _, mt := range mtsi {
		mts = append(mts, mt)
	}
	return key, mts
}

// Next returns true until the "end" of the collection is reached.  When
// the end of the collection is reached the iterator is reset back to the
// head of the collection.
func (mc *metricCatalog) Next() bool {
	mc.currentIter++
	if mc.currentIter > len(mc.keys) {
		mc.currentIter = 0
		return false
	}
	return true
}

// Subscribe atomically increments a metric's subscription count in the table.
func (mc *metricCatalog) Subscribe(ns []string, version int) error {
	mc.mutex.Lock()
	defer mc.mutex.Unlock()

	m, err := mc.get(ns, version)
	if err != nil {
		return err
	}

	m.Subscribe()
	return nil
}

// Unsubscribe atomically decrements a metric's count in the table
func (mc *metricCatalog) Unsubscribe(ns []string, version int) error {
	mc.mutex.Lock()
	defer mc.mutex.Unlock()

	m, err := mc.get(ns, version)
	if err != nil {
		return err
	}

	return m.Unsubscribe()
}

func (mc *metricCatalog) GetPlugin(mns []string, ver int) (*loadedPlugin, error) {
	m, err := mc.Get(mns, ver)
	if err != nil {
		return nil, err
	}
	return m.Plugin, nil
}

func (mc *metricCatalog) get(ns []string, ver int) (*metricType, error) {
	mts, err := mc.tree.Get(ns)
	if err != nil {
		return nil, err
	}

	if len(mts) > 1 {
		// a version IS given
		if ver >= 0 {
			l, err := getVersion(mts, ver)
			if err != nil {
				return nil, errorMetricNotFound(ns)
			}
			return l, nil
		}
		// ver is less than 0 get the latest
		return getLatest(mts), nil
	}

	//only one version so return it
	return mts[0], nil

}

func getMetricKey(metric []string) string {
	return strings.Join(metric, ".")
}

func getLatest(c []*metricType) *metricType {
	cur := c[0]
	for _, mt := range c {
		if mt.Version() > cur.Version() {
			cur = mt
		}

	}
	return cur
}

func appendIfMissing(keys []string, ns string) []string {
	for _, key := range keys {
		if ns == key {
			return keys
		}
	}
	return append(keys, ns)
}

func getVersion(c []*metricType, ver int) (*metricType, error) {
	for _, m := range c {
		if m.Plugin.Version() == ver {
			return m, nil
		}
	}
	return nil, errMetricNotFound
}
