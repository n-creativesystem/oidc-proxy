package store

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base32"
	"encoding/base64"
	"encoding/json"
	"io/ioutil"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/n-creativesystem/oidc-proxy/cache"

	"github.com/gorilla/securecookie"
	"github.com/gorilla/sessions"
)

var sessionExpire = 86400 * 30

type PluginClient interface {
	Kill()
}

type CacheStore struct {
	cache        cache.Cache
	Options      *sessions.Options
	StoreMutex   sync.RWMutex
	keyPairs     []securecookie.Codec
	pluginClient PluginClient
}

func (c *CacheStore) GetPluginClient() PluginClient {
	return c.pluginClient
}

type sessionValues map[interface{}]interface{}

func (s sessionValues) mapToJson() (string, error) {
	mp := map[string]interface{}{}
	for key, value := range s {
		mp[key.(string)] = value
	}
	buf, err := json.Marshal(&mp)
	if err != nil {
		return "", err
	}
	return string(buf), nil
}

func (s *sessionValues) jsonToMap(str string) error {
	values := sessionValues{}
	mp := map[string]interface{}{}
	if err := json.Unmarshal([]byte(str), &mp); err != nil {
		return err
	}
	for key, val := range mp {
		values[key] = val
	}
	*s = values
	return nil
}

var _ sessions.Store = &CacheStore{}

func NewStore(c cache.Cache, pluginClient PluginClient, codec ...[]byte) *CacheStore {
	store := &CacheStore{
		cache: c,
		Options: &sessions.Options{
			Path:   "/",
			MaxAge: sessionExpire,
			Secure: true,
		},
		keyPairs:     securecookie.CodecsFromPairs(codec...),
		pluginClient: pluginClient,
	}
	return store
}

func getCancelContext() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), 5*time.Second)
}

func compress(buf []byte) string {
	var buffer bytes.Buffer
	writer := gzip.NewWriter(&buffer)
	writer.Write(buf)
	writer.Flush()
	writer.Close()
	return base64.StdEncoding.EncodeToString(buffer.Bytes())
}

func decompress(compressionStr string) ([]byte, error) {
	data, err := base64.StdEncoding.DecodeString(compressionStr)
	if err != nil {
		return nil, err
	}
	rdata := bytes.NewReader(data)
	r, err := gzip.NewReader(rdata)
	if err != nil {
		return nil, err
	}
	buf, _ := ioutil.ReadAll(r)
	return buf, nil
}

func (c *CacheStore) Get(r *http.Request, name string) (*sessions.Session, error) {
	return sessions.GetRegistry(r).Get(c, name)
}

func (c *CacheStore) New(r *http.Request, name string) (*sessions.Session, error) {
	var err error
	session := sessions.NewSession(c, name)
	opts := *c.Options
	session.Options = &opts
	session.IsNew = true
	if cookie, errCookie := r.Cookie(name); errCookie == nil {
		err = securecookie.DecodeMulti(name, cookie.Value, &session.ID, c.keyPairs...)
		if err == nil {
			err := c.load(session)
			session.IsNew = !(err == nil)
		}
	}
	return session, err
}

func (c *CacheStore) Save(r *http.Request, w http.ResponseWriter, session *sessions.Session) error {
	if session.Options.MaxAge < 0 {
		if err := c.Delete(session); err != nil {
			return err
		}
		http.SetCookie(w, sessions.NewCookie(session.Name(), "", session.Options))
	} else {
		if session.ID == "" {
			session.ID = strings.TrimRight(base32.StdEncoding.EncodeToString(securecookie.GenerateRandomKey(32)), "=")
		}
		if err := c.save(session); err != nil {
			return err
		}
		encoded, err := securecookie.EncodeMulti(session.Name(), session.ID, c.keyPairs...)
		if err != nil {
			return err
		}
		http.SetCookie(w, sessions.NewCookie(session.Name(), encoded, session.Options))
	}
	return nil
}

func (c *CacheStore) save(session *sessions.Session) error {
	value := sessionValues(session.Values)
	encoded, err := value.mapToJson()
	if err != nil {
		return err
	}
	ctx, cancel := getCancelContext()
	defer cancel()
	c.StoreMutex.Lock()
	defer c.StoreMutex.Unlock()
	key := "session_" + session.ID
	return c.cache.Put(ctx, key, encoded)
}

func (c *CacheStore) load(session *sessions.Session) error {
	values := sessionValues{}
	ctx, cancel := getCancelContext()
	defer cancel()
	c.StoreMutex.Lock()
	defer c.StoreMutex.Unlock()
	key := "session_" + session.ID
	value, err := c.cache.Get(ctx, key)
	if err != nil {
		return err
	}
	err = values.jsonToMap(value)
	if err != nil {
		return err
	}
	session.Values = values
	return nil
}

func (c *CacheStore) Delete(session *sessions.Session) error {
	ctx, cancel := getCancelContext()
	defer cancel()
	c.StoreMutex.Lock()
	defer c.StoreMutex.Unlock()
	key := "session_" + session.ID
	return c.cache.Delete(ctx, key)
}
