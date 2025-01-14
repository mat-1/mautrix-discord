package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/gorilla/mux"
	"github.com/gorilla/websocket"
	log "maunium.net/go/maulogger/v2"

	"maunium.net/go/mautrix/id"

	"go.mau.fi/mautrix-discord/remoteauth"
)

const (
	SecWebSocketProtocol = "com.gitlab.beeper.discord"
)

type ProvisioningAPI struct {
	bridge *DiscordBridge
	log    log.Logger
}

func newProvisioningAPI(br *DiscordBridge) *ProvisioningAPI {
	p := &ProvisioningAPI{
		bridge: br,
		log:    br.Log.Sub("Provisioning"),
	}

	prefix := br.Config.Bridge.Provisioning.Prefix

	p.log.Debugln("Enabling provisioning API at", prefix)

	r := br.AS.Router.PathPrefix(prefix).Subrouter()

	r.Use(p.authMiddleware)

	r.HandleFunc("/disconnect", p.disconnect).Methods(http.MethodPost)
	r.HandleFunc("/ping", p.ping).Methods(http.MethodGet)
	r.HandleFunc("/login/qr", p.login).Methods(http.MethodGet)
	r.HandleFunc("/logout", p.logout).Methods(http.MethodPost)
	r.HandleFunc("/reconnect", p.reconnect).Methods(http.MethodPost)

	r.HandleFunc("/guilds", p.guildsList).Methods(http.MethodGet)
	r.HandleFunc("/guilds/{guildID}/bridge", p.guildsBridge).Methods(http.MethodPost)
	r.HandleFunc("/guilds/{guildID}/unbridge", p.guildsUnbridge).Methods(http.MethodPost)
	r.HandleFunc("/guilds/{guildID}/joinentire", p.guildsJoinEntire).Methods(http.MethodPost)

	return p
}

func jsonResponse(w http.ResponseWriter, status int, response interface{}) {
	w.Header().Add("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(response)
}

// Response structs
type Response struct {
	Success bool   `json:"success"`
	Status  string `json:"status"`
}

type Error struct {
	Success bool   `json:"success"`
	Error   string `json:"error"`
	ErrCode string `json:"errcode"`
}

// Wrapped http.ResponseWriter to capture the status code
type responseWrap struct {
	http.ResponseWriter
	statusCode int
}

var _ http.Hijacker = (*responseWrap)(nil)

func (rw *responseWrap) WriteHeader(statusCode int) {
	rw.ResponseWriter.WriteHeader(statusCode)
	rw.statusCode = statusCode
}

func (rw *responseWrap) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	hijacker, ok := rw.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, errors.New("response does not implement http.Hijacker")
	}
	return hijacker.Hijack()
}

// Middleware
func (p *ProvisioningAPI) authMiddleware(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")

		// Special case the login endpoint to use the discord qrcode auth
		if auth == "" && strings.HasSuffix(r.URL.Path, "/login") {
			authParts := strings.Split(r.Header.Get("Sec-WebSocket-Protocol"), ",")
			for _, part := range authParts {
				part = strings.TrimSpace(part)
				if strings.HasPrefix(part, SecWebSocketProtocol+"-") {
					auth = part[len(SecWebSocketProtocol+"-"):]

					break
				}
			}
		} else if strings.HasPrefix(auth, "Bearer ") {
			auth = auth[len("Bearer "):]
		}

		if auth != p.bridge.Config.Bridge.Provisioning.SharedSecret {
			jsonResponse(w, http.StatusForbidden, map[string]interface{}{
				"error":   "Invalid auth token",
				"errcode": "M_FORBIDDEN",
			})

			return
		}

		userID := r.URL.Query().Get("user_id")
		user := p.bridge.GetUserByMXID(id.UserID(userID))

		start := time.Now()
		wWrap := &responseWrap{w, 200}
		h.ServeHTTP(wWrap, r.WithContext(context.WithValue(r.Context(), "user", user)))
		duration := time.Now().Sub(start).Seconds()

		p.log.Infofln("%s %s from %s took %.2f seconds and returned status %d", r.Method, r.URL.Path, user.MXID, duration, wWrap.statusCode)
	})
}

// websocket upgrader
var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool {
		return true
	},
	Subprotocols: []string{SecWebSocketProtocol},
}

// Handlers
func (p *ProvisioningAPI) disconnect(w http.ResponseWriter, r *http.Request) {
	user := r.Context().Value("user").(*User)

	if !user.Connected() {
		jsonResponse(w, http.StatusConflict, Error{
			Error:   "You're not connected to discord",
			ErrCode: "not connected",
		})

		return
	}

	if err := user.Disconnect(); err != nil {
		jsonResponse(w, http.StatusInternalServerError, Error{
			Error:   "Failed to disconnect from discord",
			ErrCode: "failed to disconnect",
		})
	} else {
		jsonResponse(w, http.StatusOK, Response{
			Success: true,
			Status:  "Disconnected from Discord",
		})
	}
}

func (p *ProvisioningAPI) ping(w http.ResponseWriter, r *http.Request) {
	user := r.Context().Value("user").(*User)

	discord := map[string]interface{}{
		"logged_in": user.IsLoggedIn(),
		"connected": user.Connected(),
		"conn":      nil,
	}

	user.Lock()
	if user.DiscordID != "" {
		discord["id"] = user.DiscordID
	}

	if user.Session != nil {
		user.Session.Lock()
		discord["conn"] = map[string]interface{}{
			"last_heartbeat_ack":  user.Session.LastHeartbeatAck,
			"last_heartbeat_sent": user.Session.LastHeartbeatSent,
		}
		user.Session.Unlock()
	}

	resp := map[string]interface{}{
		"discord":         discord,
		"management_room": user.ManagementRoom,
		"mxid":            user.MXID,
	}

	user.Unlock()

	jsonResponse(w, http.StatusOK, resp)
}

func (p *ProvisioningAPI) logout(w http.ResponseWriter, r *http.Request) {
	user := r.Context().Value("user").(*User)
	var msg string
	if user.DiscordID != "" {
		msg = "Logged out successfully."
	} else {
		msg = "User wasn't logged in."
	}
	user.Logout()
	jsonResponse(w, http.StatusOK, Response{true, msg})
}

func (p *ProvisioningAPI) login(w http.ResponseWriter, r *http.Request) {
	userID := r.URL.Query().Get("user_id")
	user := p.bridge.GetUserByMXID(id.UserID(userID))

	c, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		p.log.Errorln("Failed to upgrade connection to websocket:", err)
		return
	}

	defer func() {
		err := c.Close()
		if err != nil {
			user.log.Debugln("Error closing websocket:", err)
		}
	}()

	go func() {
		// Read everything so SetCloseHandler() works
		for {
			_, _, err := c.ReadMessage()
			if err != nil {
				break
			}
		}
	}()

	ctx, cancel := context.WithCancel(context.Background())
	c.SetCloseHandler(func(code int, text string) error {
		user.log.Debugfln("Login websocket closed (%d), cancelling login", code)

		cancel()

		return nil
	})

	if user.IsLoggedIn() {
		c.WriteJSON(Error{
			Error:   "You're already logged into Discord",
			ErrCode: "already logged in",
		})

		return
	}

	client, err := remoteauth.New()
	if err != nil {
		user.log.Errorf("Failed to log in from provisioning API:", err)

		c.WriteJSON(Error{
			Error:   "Failed to connect to Discord",
			ErrCode: "connection error",
		})
	}

	qrChan := make(chan string)
	doneChan := make(chan struct{})

	user.log.Debugln("Started login via provisioning API")

	err = client.Dial(ctx, qrChan, doneChan)
	if err != nil {
		close(qrChan)
		close(doneChan)
	}

	for {
		select {
		case qrCode, ok := <-qrChan:
			if !ok {
				continue
			}
			c.WriteJSON(map[string]interface{}{
				"code":    qrCode,
				"timeout": 120, // TODO: move this to the library or something
			})
		case <-doneChan:
			discordUser, err := client.Result()
			if err != nil {
				c.WriteJSON(Error{
					Error:   "Failed to connect to Discord",
					ErrCode: "connection error",
				})

				p.log.Errorfln("failed to login via qrcode:", err)

				return
			}

			user.DiscordID = discordUser.UserID
			user.Update()

			if err := user.Login(discordUser.Token); err != nil {
				c.WriteJSON(Error{
					Error:   "Failed to connect to Discord",
					ErrCode: "connection error",
				})

				p.log.Errorfln("failed to login via qrcode:", err)

				return
			}

			c.WriteJSON(map[string]interface{}{
				"success": true,
				"id":      user.DiscordID,
			})

			return
		case <-ctx.Done():
			return
		}
	}
}

func (p *ProvisioningAPI) reconnect(w http.ResponseWriter, r *http.Request) {
	user := r.Context().Value("user").(*User)

	if user.Connected() {
		jsonResponse(w, http.StatusConflict, Error{
			Error:   "You're already connected to discord",
			ErrCode: "already connected",
		})

		return
	}

	if err := user.Connect(); err != nil {
		jsonResponse(w, http.StatusInternalServerError, Error{
			Error:   "Failed to connect to discord",
			ErrCode: "failed to connect",
		})
	} else {
		jsonResponse(w, http.StatusOK, Response{
			Success: true,
			Status:  "Connected to Discord",
		})
	}
}

func (p *ProvisioningAPI) guildsList(w http.ResponseWriter, r *http.Request) {
	user := r.Context().Value("user").(*User)

	var data []map[string]interface{}
	for _, userGuild := range user.GetPortals() {
		guild := p.bridge.GetGuildByID(userGuild.DiscordID, false)
		if guild == nil {
			continue
		}
		data = append(data, map[string]interface{}{
			"name": guild.Name,
			"id":   guild.ID,
			"mxid": guild.MXID,
		})
	}

	jsonResponse(w, http.StatusOK, data)
}

func (p *ProvisioningAPI) guildsBridge(w http.ResponseWriter, r *http.Request) {
	user := r.Context().Value("user").(*User)

	guildID, _ := mux.Vars(r)["guildID"]

	if err := user.bridgeGuild(guildID, false); err != nil {
		jsonResponse(w, http.StatusNotFound, Error{
			Error:   err.Error(),
			ErrCode: "M_NOT_FOUND",
		})
	} else {
		w.WriteHeader(http.StatusCreated)
	}
}

func (p *ProvisioningAPI) guildsUnbridge(w http.ResponseWriter, r *http.Request) {
	user := r.Context().Value("user").(*User)

	guildID, _ := mux.Vars(r)["guildID"]

	if err := user.unbridgeGuild(guildID); err != nil {
		jsonResponse(w, http.StatusNotFound, Error{
			Error:   err.Error(),
			ErrCode: "M_NOT_FOUND",
		})

		return
	}

	w.WriteHeader(http.StatusNoContent)
}

func (p *ProvisioningAPI) guildsJoinEntire(w http.ResponseWriter, r *http.Request) {
	user := r.Context().Value("user").(*User)

	guildID, _ := mux.Vars(r)["guildID"]

	if err := user.bridgeGuild(guildID, true); err != nil {
		jsonResponse(w, http.StatusNotFound, Error{
			Error:   err.Error(),
			ErrCode: "M_NOT_FOUND",
		})
	} else {
		w.WriteHeader(http.StatusCreated)
	}
}
