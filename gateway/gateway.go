/*
 * @Copyright Reserved By Janusec (https://www.janusec.com/).
 * @Author: U2
 * @Date: 2018-07-14 16:37:57
 * @Last Modified: U2, 2018-07-14 16:37:57
 */

package gateway

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io/ioutil"
	"net"
	"net/http"
	"net/http/httputil"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"

	"janusec/backend"
	"janusec/data"
	"janusec/firewall"
	"janusec/models"
	"janusec/usermgmt"
	"janusec/utils"

	"github.com/gorilla/sessions"
	"github.com/patrickmn/go-cache"
	"github.com/yookoala/gofast"
	"golang.org/x/net/http2"
)

var (
	store   = sessions.NewCookieStore([]byte("janusec"))
	incChan = make(chan int, 8)
	decChan = make(chan int, 8)
)

// Counter stat the concurrency requests
func Counter() {
	for {
		select {
		case <-incChan:
			concurrency++
		case <-decChan:
			concurrency--
		}
	}
}

// ReverseHandlerFunc used for reverse handler
func ReverseHandlerFunc(w http.ResponseWriter, r *http.Request) {
	fmt.Printf("\n\n>>>>> 访问URL：%s%s\n", (*r).Host,(*r).RequestURI);
	// inc concurrency
	incChan <- 1
	defer func() {
		decChan <- 1
	}()
	// r.Host may has the format: domain:port, first remove port
	index := strings.IndexByte(r.Host, ':')
	if index > 0 {
		r.Host = r.Host[0:index]
	}
	domain := backend.GetDomainByName(r.Host)
	 if domain != nil {
		fmt.Printf("      域名信息,id:%d,Name:%s,AppID:%d,CertID:%d\n", domain.ID, domain.Name, domain.AppID, domain.CertID);
	}

	if domain != nil && domain.Redirect == true {
		//找到了域名并且开启了重定向则重定向至相关的URL
		RedirectRequest(w, r, domain.Location)
		fmt.Printf("      重定向至URL:%s\n\n", domain.Location);
		return
	}
	app := backend.GetApplicationByDomain(r.Host)
	if app == nil {
		// 未找到对应的APP，则重定向到主页
		fmt.Printf("      未命中任何应用,访问主页\n\n")
		http.ServeFile(w, r, "./static/welcome/index.html")
		return
	}

	fmt.Printf("      App信息,id:%d,Name:%s,HTTPS：%t\n", app.ID, app.Name, r.TLS == nil);

	if (r.TLS == nil) && (app.RedirectHTTPS == true) {
		fmt.Printf("      访问非HTTPS，重定向至HTTPS\n\n");
		RedirectRequest(w, r, "https://"+r.Host+r.URL.Path)
		return
	}

	r.URL.Scheme = app.InternalScheme
	r.URL.Host = r.Host

	nowTimeStamp := time.Now().Unix()
	// dynamic
	srcIP := GetClientIP(r, app)
	isStatic := firewall.IsStaticResource(r)

	//检测是否属于攻击
	if app.WAFEnabled && !isStatic {
		if isCC, ccPolicy, clientID, needLog := firewall.IsCCAttack(r, app.ID, srcIP); isCC == true {
			targetURL := r.URL.Path
			if len(r.URL.RawQuery) > 0 {
				targetURL += "?" + r.URL.RawQuery
			}
			hitInfo := &models.HitInfo{TypeID: 1,
				PolicyID:  ccPolicy.AppID,
				VulnName:  "CC",
				Action:    ccPolicy.Action,
				ClientID:  clientID,
				TargetURL: targetURL,
				BlockTime: nowTimeStamp}
			switch ccPolicy.Action {
			case models.Action_Block_100:
				if needLog {
					go firewall.LogCCRequest(r, app.ID, srcIP, ccPolicy)
				}
				if app.ClientIPMethod == models.IPMethod_REMOTE_ADDR {
					go firewall.AddIP2NFTables(srcIP, ccPolicy.BlockSeconds)
				}
				GenerateBlockPage(w, hitInfo)
				return
			case models.Action_BypassAndLog_200:
				if needLog {
					go firewall.LogCCRequest(r, app.ID, srcIP, ccPolicy)
				}
			case models.Action_CAPTCHA_300:
				if needLog {
					go firewall.LogCCRequest(r, app.ID, srcIP, ccPolicy)
				}
				captchaHitInfo.Store(hitInfo.ClientID, hitInfo)
				captchaURL := CaptchaEntrance + "?id=" + hitInfo.ClientID
				http.Redirect(w, r, captchaURL, http.StatusTemporaryRedirect)
				return
			}
		}

		if isHit, policy := firewall.IsRequestHitPolicy(r, app.ID, srcIP); isHit == true {
			switch policy.Action {
			case models.Action_Block_100:
				vulnName, _ := firewall.VulnMap.Load(policy.VulnID)
				hitInfo := &models.HitInfo{TypeID: 2, PolicyID: policy.ID, VulnName: vulnName.(string)}
				go firewall.LogGroupHitRequest(r, app.ID, srcIP, policy)
				GenerateBlockPage(w, hitInfo)
				return
			case models.Action_BypassAndLog_200:
				go firewall.LogGroupHitRequest(r, app.ID, srcIP, policy)
			case models.Action_CAPTCHA_300:
				go firewall.LogGroupHitRequest(r, app.ID, srcIP, policy)
				clientID := GenClientID(r, app.ID, srcIP)
				targetURL := r.URL.Path
				if len(r.URL.RawQuery) > 0 {
					targetURL += "?" + r.URL.RawQuery
				}
				hitInfo := &models.HitInfo{TypeID: 2,
					PolicyID: policy.ID, VulnName: "Group Policy Hit",
					Action: policy.Action, ClientID: clientID,
					TargetURL: targetURL, BlockTime: nowTimeStamp}
				captchaHitInfo.Store(clientID, hitInfo)
				captchaURL := CaptchaEntrance + "?id=" + clientID
				http.Redirect(w, r, captchaURL, http.StatusTemporaryRedirect)
				return
			default:
				// models.Action_Pass_400 do nothing
			}
		}
	}

	// Check OAuth
	if app.OAuthRequired && data.CFG.PrimaryNode.OAuth.Enabled {
		session, _ := store.Get(r, "janusec-token")
		usernameI := session.Values["userid"]
		var url string
		if r.TLS != nil {
			url = "https://" + r.Host + r.URL.Path
		} else {
			url = r.URL.String()
		}
		//fmt.Println("1000", usernameI, url)
		if usernameI == nil {
			// Exec OAuth2 Authentication
			ua := r.UserAgent() //r.Header.Get("User-Agent")
			state := data.SHA256Hash(srcIP + url + ua)
			stateSession := session.Values[state]
			//fmt.Println("1001 state=", state, url)
			if stateSession == nil {
				entranceURL, err := getOAuthEntrance(state)
				if err != nil {
					_, err = w.Write([]byte(err.Error()))
					if err != nil {
						utils.DebugPrintln("w.Write error", err)
					}
					return
				}
				// Save Application URL for CallBack
				oauthState := models.OAuthState{
					CallbackURL: url,
					UserID:      ""}
				usermgmt.OAuthCache.Set(state, oauthState, cache.DefaultExpiration)
				session.Values[state] = state
				session.Options = &sessions.Options{Path: "/", MaxAge: 300}
				err = session.Save(r, w)
				if err != nil {
					utils.DebugPrintln("session.Save error", err)
				}
				//fmt.Println("1002 cache state:", oauthState, url, "307 to:", entranceURL)
				http.Redirect(w, r, entranceURL, http.StatusTemporaryRedirect)
				return
			}
			// Has state in session, get UserID from cache
			state = stateSession.(string)
			oauthStateI, found := usermgmt.OAuthCache.Get(state)
			if found == false {
				// Time expired, clear session
				session.Options = &sessions.Options{Path: "/", MaxAge: -1}
				err := session.Save(r, w)
				if err != nil {
					utils.DebugPrintln("session.Save error", err)
				}
				http.Redirect(w, r, url, http.StatusTemporaryRedirect)
				return
			}
			// found == true
			oauthState := oauthStateI.(models.OAuthState)
			if oauthState.UserID == "" {
				session.Values["userid"] = nil
				entranceURL, err := getOAuthEntrance(state)
				if err != nil {
					_, err = w.Write([]byte(err.Error()))
					if err != nil {
						utils.DebugPrintln("w.Write error", err)
					}
					return
				}
				http.Redirect(w, r, entranceURL, http.StatusTemporaryRedirect)
				return
			}
			session.Values["userid"] = oauthState.UserID
			session.Values["access_token"] = oauthState.AccessToken
			session.Options = &sessions.Options{Path: "/", MaxAge: int(app.SessionSeconds)}
			err := session.Save(r, w)
			if err != nil {
				utils.DebugPrintln("session.Save error", err)
			}
			http.Redirect(w, r, oauthState.CallbackURL, http.StatusTemporaryRedirect)
			return
		}
		// Exist username in session, Forward username to destination
		accessToken := session.Values["access_token"].(string)
		//r.Header.Set("Authorization", "Bearer "+accessToken)
		// 0.9.15 change to X-Auth-Token
		r.Header.Set("X-Auth-Token", accessToken)
		r.Header.Set("X-Auth-User", usernameI.(string))
	}

	dest := backend.SelectBackendRoute(app, r, srcIP)
	if dest == nil {
		errInfo := &models.InternalErrorInfo{
			Description: "Internal Servers Offline",
		}
		GenerateInternalErrorResponse(w, errInfo)
		return
	}

	// Modify Origin if client http and backend https
	if (r.TLS == nil) && (app.InternalScheme == "https") {
		origin := r.Header.Get("Origin")
		if len(origin) > 0 {
			r.Header.Set("Origin", "https://"+r.Host)
		}
	}

	// Add access log and statistics
	go utils.AccessLog(r.Host, r.Method, srcIP, r.RequestURI, r.UserAgent())
	go IncAccessStat(app.ID, r.URL.Path)

	if dest.RouteType == models.StaticRoute {
		// Static Web site
		staticHandler := http.FileServer(http.Dir(dest.BackendRoute))
		if strings.HasSuffix(r.URL.Path, "/") {
			targetFile := dest.BackendRoute + strings.Replace(r.URL.Path, dest.RequestRoute, "", 1) + dest.Destination
			http.ServeFile(w, r, targetFile)
			return
		}
		staticHandler.ServeHTTP(w, r)
		return
	} else if dest.RouteType == models.FastCGIRoute {
		// FastCGI
		connFactory := gofast.SimpleConnFactory("tcp", dest.Destination)
		urlPath := utils.GetRoutePath(r.URL.Path)
		newPath := r.URL.Path
		if urlPath != "/" {
			newPath = strings.Replace(r.URL.Path, dest.RequestRoute, "/", 1)
		}
		fastCGIHandler := gofast.NewHandler(
			gofast.NewFileEndpoint(dest.BackendRoute+newPath)(gofast.BasicSession),
			gofast.SimpleClientFactory(connFactory, 0),
		)
		fastCGIHandler.ServeHTTP(w, r)
		return
	}

	// var transport http.RoundTripper
	transport := &http.Transport{
		TLSHandshakeTimeout:   30 * time.Second,
		IdleConnTimeout:       30 * time.Second,
		ExpectContinueTimeout: 5 * time.Second,
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			conn, err := net.Dial("tcp", dest.Destination)
			dest.CheckTime = nowTimeStamp
			if err != nil {
				dest.Online = false
				utils.DebugPrintln("DialContext error", err)
				errInfo := &models.InternalErrorInfo{
					Description: "Internal Server Offline",
				}
				GenerateInternalErrorResponse(w, errInfo)
			}
			return conn, err
		},
		DialTLS: func(network, addr string) (net.Conn, error) {
			conn, err := net.Dial("tcp", dest.Destination)
			dest.CheckTime = nowTimeStamp
			if err != nil {
				dest.Online = false
				dest.CheckTime = nowTimeStamp
				utils.DebugPrintln("DialContext error", err)
				errInfo := &models.InternalErrorInfo{
					Description: "Internal Server Offline",
				}
				GenerateInternalErrorResponse(w, errInfo)
				return nil, err
			}
			cfg := &tls.Config{
				ServerName:         r.Host,
				NextProtos:         []string{"h2", "http/1.1"},
				MinVersion:         tls.VersionTLS12,
				InsecureSkipVerify: true,
			}
			tlsConn := tls.Client(conn, cfg)
			if err := tlsConn.Handshake(); err != nil {
				//utils.DebugPrintln("tlsConn.Handshake error", err, tlsConn)
				//_ = conn.Close()
				//return nil, err
			}
			return tlsConn, err //net.Dial("tcp", dest)
		},
	}
	err := http2.ConfigureTransport(transport)
	if err != nil {
		utils.DebugPrintln("http2.ConfigureTransport error", err)
	}

	// Check static cache
	if isStatic {
		// First check Header Range, not cache for range
		rangeValue := r.Header.Get("Range")
		if rangeValue == "" {
			staticRoot := fmt.Sprintf("./static/cdncache/%d", app.ID)
			targetFile := staticRoot + r.URL.Path
			// Check Static Cache
			fi, err := os.Stat(targetFile)
			if err == nil {
				// Found targetFile
				now := time.Now()
				fiStat := fi.Sys().(*syscall.Stat_t)
				// Use ctime fiStat.Ctim.Sec to mark the last check time
				pastSeconds := now.Unix() - fiStat.Ctim.Sec
				if pastSeconds > 1800 {
					// check update
					backendAddr := fmt.Sprintf("%s://%s%s", app.InternalScheme, dest.Destination, r.RequestURI)
					req, err := http.NewRequest("GET", backendAddr, nil)
					if err != nil {
						utils.DebugPrintln("Check Update NewRequest", err)
					}
					if err == nil {
						req.Header.Set("Host", r.Host)
						modTimeGMT := fi.ModTime().UTC().Format(http.TimeFormat)
						//If-Modified-Since: Sun, 14 Jun 2020 13:54:20 GMT
						req.Header.Set("If-Modified-Since", modTimeGMT)
						client := http.Client{
							Transport: transport,
						}
						resp, err := client.Do(req)
						if err != nil {
							utils.DebugPrintln("Cache update Do", err)
							return
						}
						defer resp.Body.Close()
						if resp.StatusCode == http.StatusOK {
							//fmt.Println("200", backendAddr)
							bodyBuf, _ := ioutil.ReadAll(resp.Body)
							err = ioutil.WriteFile(targetFile, bodyBuf, 0600)
							lastModified, err := time.Parse(http.TimeFormat, resp.Header.Get("Last-Modified"))
							if err != nil {
								//utils.DebugPrintln("CDN Parse Last-Modified", targetFile, err)
							}
							err = os.Chtimes(targetFile, now, lastModified)
							if err != nil {
								utils.DebugPrintln("CDN Chtimes", targetFile, err)
							}
						} else if resp.StatusCode == http.StatusNotModified {
							//fmt.Println("304", backendAddr)
							err := os.Chtimes(targetFile, now, fi.ModTime())
							if err != nil {
								utils.DebugPrintln("Cache update access time", err)
							}
						}
					}
				}
				http.ServeFile(w, r, targetFile)
				return
			}
		}
		// Has Range Header, or resource Not Found, Continue
	}

	// Reverse Proxy
	proxy := &httputil.ReverseProxy{
		Director: func(req *http.Request) {
			//req.URL.Scheme = app.InternalScheme
			//req.URL.Host = r.Host
		},
		Transport:      transport,
		ModifyResponse: rewriteResponse}
	if utils.Debug {
		//dump, err := httputil.DumpRequest(r, true)
		//utils.CheckError("ReverseHandlerFunc DumpRequest", err)
		//fmt.Println(string(dump))
	}
	proxy.ServeHTTP(w, r)
}

func getOAuthEntrance(state string) (entranceURL string, err error) {
	switch data.CFG.PrimaryNode.OAuth.Provider {
	case "wxwork":
		entranceURL = fmt.Sprintf("https://open.work.weixin.qq.com/wwopen/sso/qrConnect?appid=%s&agentid=%s&redirect_uri=%s&state=%s",
			data.CFG.PrimaryNode.OAuth.Wxwork.CorpID,
			data.CFG.PrimaryNode.OAuth.Wxwork.AgentID,
			data.CFG.PrimaryNode.OAuth.Wxwork.Callback,
			state)
	case "dingtalk":
		entranceURL = fmt.Sprintf("https://oapi.dingtalk.com/connect/qrconnect?appid=%s&response_type=code&scope=snsapi_login&state=%s&redirect_uri=%s",
			data.CFG.PrimaryNode.OAuth.Dingtalk.AppID,
			state,
			data.CFG.PrimaryNode.OAuth.Dingtalk.Callback)
	case "feishu":
		entranceURL = fmt.Sprintf("https://open.feishu.cn/open-apis/authen/v1/index?redirect_uri=%s&app_id=%s&state=%s",
			data.CFG.PrimaryNode.OAuth.Feishu.Callback,
			data.CFG.PrimaryNode.OAuth.Feishu.AppID,
			state)
	case "ldap":
		entranceURL = "/ldap/login?state=" + state
	case "cas2":
		entranceURL = fmt.Sprintf("%s/login?renew=true&service=%s?state=%s",
			data.CFG.PrimaryNode.OAuth.CAS2.Entrance, data.CFG.PrimaryNode.OAuth.CAS2.Callback, state)
	case "saml":
		entranceURL = "/saml/login?state=" + state
	default:
		//w.Write([]byte("Designated OAuth not supported, please check config.json ."))
		return "", errors.New("the OAuth provider is not supported, please check config.json")
	}
	return entranceURL, nil
}

// RedirectRequest for example: redirect 80 to 443
func RedirectRequest(w http.ResponseWriter, r *http.Request, location string) {
	if len(r.URL.RawQuery) > 0 {
		location += "?" + r.URL.RawQuery
	}
	http.Redirect(w, r, location, http.StatusMovedPermanently)
}

// GenClientID generate unique client id
func GenClientID(r *http.Request, appID int64, srcIP string) string {
	preHashContent := srcIP
	url := r.URL.Path
	preHashContent += url
	ua := r.Header.Get("User-Agent")
	preHashContent += ua
	cookie := r.Header.Get("Cookie")
	preHashContent += cookie
	clientID := data.SHA256Hash(preHashContent)
	return clientID
}

// GetClientIP acquire the client IP address
func GetClientIP(r *http.Request, app *models.Application) (clientIP string) {
	switch app.ClientIPMethod {
	case models.IPMethod_REMOTE_ADDR:
		clientIP, _, _ = net.SplitHostPort(r.RemoteAddr)
		return clientIP
	case models.IPMethod_X_FORWARDED_FOR:
		xForwardedFor := r.Header.Get("X-Forwarded-For")
		ips := strings.Split(xForwardedFor, ", ")
		clientIP = ips[len(ips)-1]
	case models.IPMethod_X_REAL_IP:
		clientIP = r.Header.Get("X-Real-IP")
	case models.IPMethod_REAL_IP:
		clientIP = r.Header.Get("Real-IP")
	}
	if len(clientIP) == 0 {
		clientIP, _, _ = net.SplitHostPort(r.RemoteAddr)
	}
	return clientIP
}

func OAuthLogout(w http.ResponseWriter, r *http.Request) {
	session, _ := store.Get(r, "janusec-token")
	session.Options = &sessions.Options{Path: "/", MaxAge: -1}
	err := session.Save(r, w)
	if err != nil {
		utils.DebugPrintln("OAuthLogout session.Save error", err)
	}
	http.Redirect(w, r, "/", http.StatusTemporaryRedirect)
}

// DailyRoutineTasks for clear expired logs
func DailyRoutineTasks() {
	for {
		now := time.Now()
		next := now.Add(time.Hour * 24)
		next = time.Date(next.Year(), next.Month(), next.Day(), 3, 0, 0, 0, next.Location()) // AM 03:00
		t := time.NewTimer(next.Sub(now))
		<-t.C

		// Clear expired logs under ./log/
		globalSettings := data.GetGlobalSettings2()
		expiredDays := "+" + strconv.FormatInt(globalSettings.AccessLogDays, 10)
		cmd := exec.Command("find", "./log/", "-mtime", expiredDays, "-delete")
		err := cmd.Run()
		if err != nil {
			utils.DebugPrintln("Delete old log files Error:", err)
		}
	}
}
