package jwt

import (
	"errors"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"scanorder/internal/config"
)

// Claims JWT声明
type Claims struct {
	UserID   string `json:"user_id"`
	StoreID  string `json:"store_id"`
	UserType string `json:"user_type"` // public, staff, admin
	jwt.RegisteredClaims
}

// JWT JWT工具
type JWT struct {
	secret     []byte
	expireHour int
}

// NewJWT 创建JWT实例
func NewJWT(cfg *config.JWTConfig) *JWT {
	return &JWT{
		secret:     []byte(cfg.Secret),
		expireHour: cfg.ExpireHour,
	}
}

// GenerateToken 生成JWT token
func (j *JWT) GenerateToken(userID, storeID, userType string) (string, error) {
	now := time.Now()
	claims := Claims{
		UserID:   userID,
		StoreID:  storeID,
		UserType: userType,
		RegisteredClaims: jwt.RegisteredClaims{
			ExpiresAt: jwt.NewNumericDate(now.Add(time.Hour * time.Duration(j.expireHour))),
			IssuedAt:  jwt.NewNumericDate(now),
			NotBefore: jwt.NewNumericDate(now),
			Issuer:    "scanorder",
		},
	}

	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	return token.SignedString(j.secret)
}

// ParseToken 解析JWT token
func (j *JWT) ParseToken(tokenString string) (*Claims, error) {
	token, err := jwt.ParseWithClaims(tokenString, &Claims{}, func(token *jwt.Token) (interface{}, error) {
		if _, ok := token.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, errors.New("unexpected signing method")
		}
		return j.secret, nil
	})

	if err != nil {
		return nil, err
	}

	if claims, ok := token.Claims.(*Claims); ok && token.Valid {
		return claims, nil
	}

	return nil, errors.New("invalid token")
}

// RefreshToken 刷新token
func (j *JWT) RefreshToken(tokenString string) (string, error) {
	claims, err := j.ParseToken(tokenString)
	if err != nil {
		return "", err
	}

	// 重新生成token
	return j.GenerateToken(claims.UserID, claims.StoreID, claims.UserType)
}

// ValidateToken 验证token是否有效
func (j *JWT) ValidateToken(tokenString string) (*Claims, error) {
	return j.ParseToken(tokenString)
}
