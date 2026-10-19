package web

import (
	"net/http"
	"strings"
	"unicode/utf8"

	"blog-my/internal/model"

	"github.com/gin-gonic/gin"
	"golang.org/x/crypto/bcrypt"
)

func pageIndex(c *gin.Context) {
	var u model.User
	if err := model.DB.Where("id = ?", currentUserID(c)).First(&u).Error; err != nil {
		clearSession(c)
		c.Redirect(http.StatusFound, "/login")
		return
	}
	c.HTML(http.StatusOK, "chat/index.html", gin.H{
		"Title":    "聊天",
		"Username": u.Username,
	})
}

func pageLogin(c *gin.Context) {
	c.HTML(http.StatusOK, "user/login.html", gin.H{"Title": "登录", "Error": ""})
}

func postLogin(c *gin.Context) {
	username := strings.TrimSpace(c.PostForm("username"))
	password := c.PostForm("password")

	var u model.User
	err := model.DB.Where("username = ?", username).First(&u).Error
	if err != nil || bcrypt.CompareHashAndPassword([]byte(u.Password), []byte(password)) != nil {
		c.HTML(http.StatusOK, "user/login.html", gin.H{"Title": "登录", "Error": "用户名或密码错误"})
		return
	}

	setSession(c, u.Id)
	c.Redirect(http.StatusFound, "/")
}

func pageRegister(c *gin.Context) {
	c.HTML(http.StatusOK, "user/register.html", gin.H{"Title": "注册", "Error": ""})
}

func postRegister(c *gin.Context) {
	username := strings.TrimSpace(c.PostForm("username"))
	password := c.PostForm("password")

	if n := utf8.RuneCountInString(username); n < 3 || n > 32 {
		c.HTML(http.StatusOK, "user/register.html", gin.H{"Title": "注册", "Error": "用户名长度需为 3~32 个字符"})
		return
	}
	if utf8.RuneCountInString(password) < 6 {
		c.HTML(http.StatusOK, "user/register.html", gin.H{"Title": "注册", "Error": "密码至少 6 位"})
		return
	}

	var existing model.User
	if err := model.DB.Where("username = ?", username).First(&existing).Error; err == nil {
		c.HTML(http.StatusOK, "user/register.html", gin.H{"Title": "注册", "Error": "用户名已存在"})
		return
	}

	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		c.HTML(http.StatusOK, "user/register.html", gin.H{"Title": "注册", "Error": "注册失败，请稍后重试"})
		return
	}

	u := model.User{Username: username, Password: string(hash)}
	if err := model.DB.Create(&u).Error; err != nil {
		msg := "注册失败，请稍后重试"
		if strings.Contains(strings.ToLower(err.Error()), "duplicate") {
			msg = "用户名已存在"
		}
		c.HTML(http.StatusOK, "user/register.html", gin.H{"Title": "注册", "Error": msg})
		return
	}

	setSession(c, u.Id)
	c.Redirect(http.StatusFound, "/")
}

func logout(c *gin.Context) {
	clearSession(c)
	c.Redirect(http.StatusFound, "/login")
}

func pageSettings(c *gin.Context) {
	var u model.User
	if err := model.DB.Where("id = ?", currentUserID(c)).First(&u).Error; err != nil {
		clearSession(c)
		c.Redirect(http.StatusFound, "/login")
		return
	}
	c.HTML(http.StatusOK, "user/settings.html", gin.H{
		"Title":    "API Key 设置",
		"Username": u.Username,
	})
}
