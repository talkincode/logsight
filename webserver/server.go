package webserver

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"path"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/360EntSecGroup-Skylar/excelize"
	"github.com/gocarina/gocsv"
	_ "github.com/gocarina/gocsv"
	"github.com/golang-jwt/jwt/v4"
	"github.com/labstack/echo-jwt/v4"
	"github.com/spf13/cast"
	"github.com/talkincode/logsight/app"
	"github.com/talkincode/logsight/assets"
	"github.com/talkincode/logsight/common"
	"github.com/talkincode/logsight/common/excel"
	"github.com/talkincode/logsight/common/tpl"
	"github.com/talkincode/logsight/common/web"
	"github.com/talkincode/logsight/common/zaplog/log"
	"github.com/talkincode/logsight/models"

	"github.com/gorilla/sessions"
	"github.com/labstack/echo-contrib/session"
	"github.com/labstack/echo/v4"
	"github.com/labstack/echo/v4/middleware"
	elog "github.com/labstack/gommon/log"
	"github.com/pkg/errors"
)

const UserSession = "logsight_user_session"
const UserSessionName = "logsight_user_session_name"
const UserSessionLevel = "logsight_user_session_level"
const ConstCookieName = "logsight_cookie"

var (
	SessionSkipPrefix = []string{
		"/ready",
		"/realip",
		"/api",
		"/login",
		"/admin/login",
		"/radius/accounting/add",
		"/static",
	}
	JwtSkipPrefix = []string{
		"/ready",
		"/realip",
		"/login",
		"/admin/login",
		"/radius/accounting/add",
		"/static",
	}
)

var server *AdminServer

type AdminServer struct {
	root      *echo.Echo
	api       *echo.Group
	jwtConfig echojwt.Config
}

func Init() {
	server = NewAdminServer()
}

func Listen() error {
	return server.Start()
}

// NewAdminServer 创建管理系统服务器
func NewAdminServer() *AdminServer {
	appconfig := app.GConfig()
	s := &AdminServer{}
	s.root = echo.New()
	s.root.Pre(middleware.RemoveTrailingSlash())
	s.root.Use(middleware.GzipWithConfig(middleware.GzipConfig{
		Skipper: func(c echo.Context) bool {
			return strings.HasPrefix(c.Path(), "/metrics")
		},
		Level: 1,
	}))
	// 失败恢复处理中间件
	s.root.Use(ServerRecover(appconfig.System.Debug))
	// 日志处理中间件
	s.root.Use(middleware.LoggerWithConfig(middleware.LoggerConfig{
		Format: appconfig.System.Appid + " ${time_rfc3339} ${remote_ip} ${method} ${uri} ${protocol} ${status} ${id} ${user_agent} ${latency} ${bytes_in} ${bytes_out} ${error}\n",
		Output: os.Stdout,
	}))
	// p := prometheus.NewPrometheus("logsight", nil)
	// p.Use(s.root)

	// session 中间件， 采用 Cookie 存储方式
	sessStore := sessions.NewCookieStore([]byte(appconfig.Web.Secret))
	sessStore.MaxAge(3600 * 24)
	s.root.Use(session.Middleware(sessStore))
	s.root.Use(sessionCheck())

	// 静态目录映射
	ffs, _ := fs.Sub(assets.StaticFs, "static")
	s.root.StaticFS("/static/*", ffs)
	// 模板加载
	s.root.Renderer = tpl.NewCommonTemplate(assets.TemplatesFs, []string{"templates"}, app.GApp().GetTemplateFuncMap())

	s.root.HideBanner = true
	// 设置日志级别
	s.root.Logger.SetLevel(common.If(appconfig.System.Debug, elog.DEBUG, elog.INFO).(elog.Lvl))
	s.root.Debug = appconfig.System.Debug

	s.root.GET("/ready", func(c echo.Context) error {
		return c.JSON(200, web.RestSucc("OK"))
	})

	s.root.GET("/realip", func(c echo.Context) error {
		return c.String(200, c.RealIP())
	})

	// JWT 中间件
	s.jwtConfig = echojwt.Config{
		SigningKey:    []byte(appconfig.Web.Secret),
		SigningMethod: middleware.AlgorithmHS256,
		Skipper:       jwtSkipFunc(),
		ErrorHandler: func(c echo.Context, err error) error {
			return c.JSON(http.StatusBadRequest, web.RestError("Resource access is limited "+err.Error()))
		},
	}
	return s
}

// Start Admin Server
func (s *AdminServer) Start() error {
	appconfig := app.GConfig()
	go func() {
		log.Infof("Prepare to start the TLS management port %s:%d", appconfig.Web.Host, appconfig.Web.TlsPort)
		err := s.root.StartTLS(fmt.Sprintf("%s:%d", appconfig.Web.Host, appconfig.Web.TlsPort),
			path.Join(appconfig.GetPrivateDir(), "logsight.tls.crt"), path.Join(appconfig.GetPrivateDir(), "logsight.tls.key"))
		if err != nil {
			log.Errorf("Error starting TLS management port %s", err.Error())
		}
	}()
	log.Infof("Start the management server %s:%d", appconfig.Web.Host, appconfig.Web.Port)
	err := s.root.Start(fmt.Sprintf("%s:%d", appconfig.Web.Host, appconfig.Web.Port))
	if err != nil {
		log.Errorf("Error starting management server %s", err.Error())
	}
	return err
}

// ParseJwtToken 解析 Jwt Token
func (s *AdminServer) ParseJwtToken(tokenstr string) (jwt.MapClaims, error) {
	config := s.jwtConfig
	token, err := jwt.Parse(tokenstr, func(t *jwt.Token) (interface{}, error) {
		// Check the signing method
		if t.Method.Alg() != config.SigningMethod {
			return nil, fmt.Errorf("unexpected jwt signing method=%v", t.Header["alg"])
		}
		if len(config.SigningKeys) > 0 {
			if kid, ok := t.Header["kid"].(string); ok {
				if key, ok := config.SigningKeys[kid]; ok {
					return key, nil
				}
			}
			return nil, fmt.Errorf("unexpected jwt key id=%v", t.Header["kid"])
		}
		return config.SigningKey, nil
	})
	if err != nil {
		return nil, err
	}
	claims := token.Claims.(jwt.MapClaims)
	return claims, err
}

func (s *AdminServer) WrapJwtHandler(h http.Handler) echo.HandlerFunc {
	return func(c echo.Context) error {
		var token string
		cookie, err := c.Cookie(ConstCookieName)
		if err != nil {
			token = c.QueryParam("t")
		} else {
			token = cookie.Value
		}
		_, err = s.ParseJwtToken(token)
		if err != nil {
			return fmt.Errorf("token error")
		}
		h.ServeHTTP(c.Response(), c.Request())
		return nil
	}
}

// ServerRecover Web 服务恢复处理中间件
func ServerRecover(debug bool) echo.MiddlewareFunc {
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			defer func() {
				if r := recover(); r != nil {
					err, ok := r.(error)
					if !ok {
						err = fmt.Errorf("%v", r)
					}
					if debug {
						log.Errorf("%+v", errors.WithStack(err))
					}
					c.Error(echo.NewHTTPError(http.StatusInternalServerError, err.Error()))
				}
			}()
			return next(c)
		}
	}
}

// skipFUnc Web 请求过滤中间件
func jwtSkipFunc() func(c echo.Context) bool {
	return func(c echo.Context) bool {
		if os.Getenv("TEAMSACS_DEVMODE") == "true" {
			return true
		}

		for _, prefix := range JwtSkipPrefix {
			if strings.HasPrefix(c.Path(), prefix) {
				return true
			}
		}
		return false
	}
}

// 检查 Session
func sessionCheck() echo.MiddlewareFunc {
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			if c.RealIP() == "127.0.0.1" && strings.HasPrefix(c.Path(), "/dbapi") {
				return next(c)
			}

			for _, prefix := range SessionSkipPrefix {
				if strings.HasPrefix(c.Path(), prefix) {
					return next(c)
				}
			}
			sess, _ := session.Get(UserSession, c)
			username := sess.Values[UserSessionName]
			if username == nil || username == "" {
				return c.Redirect(http.StatusTemporaryRedirect, "/login?errmsg=User not logged in or login expired")
			}
			return next(c)
		}
	}
}

func GetCurrUser(c echo.Context) *models.SysOpr {
	sess, _ := session.Get(UserSession, c)
	username := sess.Values[UserSessionName]
	if username == nil || username == "" {
		panic("用户未登录")
	}
	user := models.SysOpr{}
	err := app.GApp().DB().Where("username = ?", username).First(&user).Error
	common.Must(err)
	return &user
}

func GetCurrUserlevel(c echo.Context) string {
	sess, _ := session.Get(UserSession, c)
	level := sess.Values[UserSessionLevel]
	if level == nil || level == "" {
		panic("用户未登录")
	}
	return level.(string)
}

func PubOpLog(c echo.Context, message string) {
	sess, _ := session.Get(UserSession, c)
	username := sess.Values[UserSessionName]
	if username == nil || username == "" {
		return
	}
	app.GApp().DB().Create(&models.SysOprLog{
		ID:        common.UUIDint64(),
		OprName:   username.(string),
		OprIp:     c.Path(),
		OptAction: c.RealIP(),
		OptDesc:   message,
		OptTime:   time.Now(),
	})
}

// ImportData Import the file contents
func ImportData(c echo.Context, sheet string) ([]map[string]interface{}, error) {
	file, err := c.FormFile("upload")
	if err != nil {
		return nil, err
	}
	src, err := file.Open()
	if err != nil {
		return nil, err
	}
	defer src.Close()
	var items []map[string]interface{}
	if strings.HasSuffix(file.Filename, "json") {
		items, err = web.ReadImportJsonData(src)
		if err != nil {
			return nil, err
		}
	} else if strings.HasSuffix(file.Filename, "csv") {
		items, err = web.ReadImportCsvData(src)
		if err != nil {
			return nil, err
		}
	} else {
		items, err = web.ReadImportExcelData(src, sheet)
		if err != nil {
			return nil, err
		}
	}
	var datas = make([]map[string]interface{}, 0)
	for _, item := range items {
		_id, ok := item["id"]
		if !ok || common.IsEmptyOrNA(cast.ToString(_id)) {
			_id, ok = item["ID"]
		}
		if !ok || common.IsEmptyOrNA(cast.ToString(_id)) {
			_id, ok = item["Id"]
		}
		if !ok || common.IsEmptyOrNA(cast.ToString(_id)) {
			item["id"] = strconv.FormatInt(common.UUIDint64(), 10)
		}
		datas = append(datas, item)
	}
	return datas, nil
}

func ExportData(c echo.Context, data []map[string]interface{}, sheet string) error {
	filename := fmt.Sprintf("%s-%d.xlsx", sheet, common.UUIDint64())
	filepath := path.Join(app.GConfig().GetDataDir(), filename)
	xlsx := excelize.NewFile()
	index := xlsx.NewSheet(sheet)
	names := make([]string, 0)
	for i, item := range data {
		if i == 0 {
			for k, _ := range item {
				names = append(names, k)
			}
			sort.Slice(names, func(i, j int) bool {
				return names[i] == "id"
			})
			for j, name := range names {
				xlsx.SetCellValue(sheet, fmt.Sprintf("%s%d", excel.COLNAMES[j], 1), name)
			}
		}
		for j, name := range names {
			_value := cast.ToString(item[name])
			xlsx.SetCellValue(sheet, fmt.Sprintf("%s%d", excel.COLNAMES[j], i+2), _value)
		}
	}
	xlsx.SetActiveSheet(index)
	err := xlsx.SaveAs(filepath)
	if err != nil {
		return err
	}
	c.Response().Header().Set("Content-Disposition", fmt.Sprintf("attachment;filename=%s.xlsx", sheet))
	return c.File(filepath)
}

func ExportCsv(c echo.Context, v interface{}, name string) error {
	filename := fmt.Sprintf("%s-%d.csv", name, common.UUIDint64())
	filepath := path.Join(app.GConfig().GetDataDir(), filename)
	nfs, err := os.Create(filepath)
	defer nfs.Close()
	if err != nil {
		return err
	}
	err = gocsv.Marshal(v, nfs)
	if err != nil {
		return err
	}
	c.Response().Header().Set("Content-Disposition", fmt.Sprintf("attachment;filename=%s.csv", name))
	return c.File(filepath)
}

func ExportJson(c echo.Context, v interface{}, name string) error {
	bs, err := json.Marshal(v)
	if err != nil {
		return err
	}
	c.Response().Header().Set("Content-Disposition", fmt.Sprintf("attachment;filename=%s.json", name))
	return c.JSONBlob(http.StatusOK, bs)
}

func GET(path string, h echo.HandlerFunc, m ...echo.MiddlewareFunc) *echo.Route {
	log.Debugf("Add GET Router %s", path)
	return server.root.GET(path, h, m...)
}

func POST(path string, h echo.HandlerFunc, m ...echo.MiddlewareFunc) *echo.Route {
	log.Debugf("Add POST Router %s", path)
	return server.root.POST(path, h, m...)
}

func PUT(path string, h echo.HandlerFunc, m ...echo.MiddlewareFunc) *echo.Route {
	log.Debugf("Add PUT Router %s", path)
	return server.root.PUT(path, h, m...)
}

func DELETE(path string, h echo.HandlerFunc, m ...echo.MiddlewareFunc) *echo.Route {
	log.Debugf("Add DELETE Router %s", path)
	return server.root.DELETE(path, h, m...)
}
