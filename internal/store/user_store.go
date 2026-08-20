package store

import (
	"strings"

	"mail_go/internal/db"

	"golang.org/x/crypto/bcrypt"
	"gorm.io/gorm"
)

// UserStore defines the interface for user data operations.
type UserStore interface {
	Create(user *db.User) error
	GetByID(id uint) (*db.User, error)
	GetByUsername(username string, domainID uint) (*db.User, error)
	GetByEmail(email string) (*db.User, error)
	Authenticate(email, password string) (*db.User, error)
	// AuthenticateLogin 协议层登录（IMAP/SMTP/POP3）：与 Authenticate 相同，
	// 但支持裸用户名（如 "kevin"），自动解析到其唯一所属域名；多域名下
	// 用户名存在歧义时要求完整邮箱。兼容手机/客户端只填用户名的配置。
	AuthenticateLogin(login, password string) (*db.User, error)
	// LoginExists 判断登录名（完整邮箱或裸用户名）是否对应系统中的用户，
	// 供封禁逻辑区分“真实用户输错密码”（保留宽限）与“枚举型爆破”
	// （跳过宽限，见 RecordAuthFailure 的 knownUser 参数）。
	LoginExists(login string) bool
	Update(user *db.User) error
	Delete(id uint) error
	List(domainID uint, page, size int) ([]db.User, int64, error)
	ListAll(page, size int) ([]db.User, int64, error)
	UpdateUsedBytes(id uint, delta int64) error
	UpdatePassword(userID uint, hashedPassword string) error
	// TryReserveQuota 原子预扣 delta 字节：仅在不超过配额时生效并返回 true，
	// 否则不做任何修改返回 false。防止并发提交绕过配额检查（TOCTOU）。
	TryReserveQuota(userID uint, delta int64) (bool, error)
}

// userStoreGorm implements UserStore using GORM.
type userStoreGorm struct {
	db *gorm.DB
}

// newUserStore creates a new GORM-backed UserStore.
func newUserStore(database *gorm.DB) UserStore {
	return &userStoreGorm{db: database}
}

// Create inserts a new user record.
func (s *userStoreGorm) Create(user *db.User) error {
	return s.db.Create(user).Error
}

// GetByID retrieves a user by primary key.
func (s *userStoreGorm) GetByID(id uint) (*db.User, error) {
	var user db.User
	if err := s.db.Preload("Domain").First(&user, id).Error; err != nil {
		return nil, err
	}
	return &user, nil
}

// GetByUsername retrieves a user by username and domain ID.
func (s *userStoreGorm) GetByUsername(username string, domainID uint) (*db.User, error) {
	var user db.User
	if err := s.db.Where("username = ? AND domain_id = ?", username, domainID).First(&user).Error; err != nil {
		return nil, err
	}
	return &user, nil
}

// GetByEmail retrieves a user by email address (user@domain format).
func (s *userStoreGorm) GetByEmail(email string) (*db.User, error) {
	parts := strings.SplitN(email, "@", 2)
	if len(parts) != 2 {
		return nil, ErrInvalidEmail
	}
	username := parts[0]
	domainName := parts[1]

	var user db.User
	if err := s.db.Joins("JOIN domains ON domains.id = users.domain_id").
		Where("users.username = ? AND domains.name = ?", username, domainName).
		Preload("Domain").
		First(&user).Error; err != nil {
		return nil, err
	}
	return &user, nil
}

// Authenticate verifies an email/password combination and returns the user on success.
func (s *userStoreGorm) Authenticate(email, password string) (*db.User, error) {
	user, err := s.GetByEmail(email)
	if err != nil {
		return nil, ErrInvalidCredentials
	}
	if !user.IsActive {
		return nil, ErrUserInactive
	}
	if err := bcrypt.CompareHashAndPassword([]byte(user.PasswordHash), []byte(password)); err != nil {
		return nil, ErrInvalidCredentials
	}
	return user, nil
}

// AuthenticateLogin 协议层登录：优先按完整邮箱认证；裸用户名（无 @）时
// 按用户名全局查找，仅在唯一归属时接受（多域名同名视为歧义，返回失败，
// 客户端应改用完整邮箱）。密码校验与 IsActive 逻辑与 Authenticate 一致。
func (s *userStoreGorm) AuthenticateLogin(login, password string) (*db.User, error) {
	if strings.Contains(login, "@") {
		return s.Authenticate(login, password)
	}

	var users []db.User
	if err := s.db.Joins("JOIN domains ON domains.id = users.domain_id").
		Where("users.username = ?", login).
		Preload("Domain").
		Find(&users).Error; err != nil {
		return nil, ErrInvalidCredentials
	}
	if len(users) != 1 {
		// 0 个：用户不存在；多个：跨域名同名歧义，要求完整邮箱
		return nil, ErrInvalidCredentials
	}
	user := users[0]
	if !user.IsActive {
		return nil, ErrUserInactive
	}
	if err := bcrypt.CompareHashAndPassword([]byte(user.PasswordHash), []byte(password)); err != nil {
		return nil, ErrInvalidCredentials
	}
	return &user, nil
}

// LoginExists 判断登录名是否对应系统中的用户：完整邮箱按邮箱查，
// 裸用户名按用户名全局查（存在即算，歧义不影响存在性判定）。
// 仅用于封禁分级（knownUser），不做认证。
func (s *userStoreGorm) LoginExists(login string) bool {
	login = strings.TrimSpace(login)
	if login == "" {
		return false
	}
	if strings.Contains(login, "@") {
		_, err := s.GetByEmail(login)
		return err == nil
	}
	var count int64
	if err := s.db.Model(&db.User{}).Where("username = ?", login).Count(&count).Error; err != nil {
		return false
	}
	return count > 0
}

// Update saves changes to an existing user record.
func (s *userStoreGorm) Update(user *db.User) error {
	return s.db.Save(user).Error
}

// Delete removes a user by ID (soft delete if supported, hard delete otherwise).
func (s *userStoreGorm) Delete(id uint) error {
	return s.db.Delete(&db.User{}, id).Error
}

// List retrieves a paginated list of users for a given domain.
func (s *userStoreGorm) List(domainID uint, page, size int) ([]db.User, int64, error) {
	var users []db.User
	var total int64

	query := s.db.Where("domain_id = ?", domainID)
	if err := query.Model(&db.User{}).Count(&total).Error; err != nil {
		return nil, 0, err
	}

	offset := (page - 1) * size
	if err := s.db.Preload("Domain").Where("domain_id = ?", domainID).Offset(offset).Limit(size).Find(&users).Error; err != nil {
		return nil, 0, err
	}
	return users, total, nil
}

// UpdateUsedBytes atomically adjusts the UsedBytes field by delta.
func (s *userStoreGorm) UpdateUsedBytes(id uint, delta int64) error {
	return s.db.Model(&db.User{}).Where("id = ?", id).
		Update("used_bytes", gorm.Expr("used_bytes + ?", delta)).Error
}

// TryReserveQuota atomically reserves delta bytes for a user within quota.
// The reservation is applied (used_bytes incremented) only when it does not
// exceed quota_bytes; otherwise no change is made and false is returned.
func (s *userStoreGorm) TryReserveQuota(userID uint, delta int64) (bool, error) {
	if delta <= 0 {
		return false, nil
	}
	res := s.db.Model(&db.User{}).
		Where("id = ? AND used_bytes + ? <= quota_bytes", userID, delta).
		Update("used_bytes", gorm.Expr("used_bytes + ?", delta))
	if res.Error != nil {
		return false, res.Error
	}
	return res.RowsAffected == 1, nil
}

// UpdatePassword updates the password hash for a user and clears the
// must-change-password flag (the user has now set their own password).
func (s *userStoreGorm) UpdatePassword(userID uint, hashedPassword string) error {
	return s.db.Model(&db.User{}).Where("id = ?", userID).
		Updates(map[string]interface{}{
			"password_hash":         hashedPassword,
			"must_change_password": false,
		}).Error
}

// ListAll retrieves a paginated list of all users across all domains.
func (s *userStoreGorm) ListAll(page, size int) ([]db.User, int64, error) {
	var users []db.User
	var total int64

	if err := s.db.Model(&db.User{}).Count(&total).Error; err != nil {
		return nil, 0, err
	}

	offset := (page - 1) * size
	if err := s.db.Preload("Domain").Offset(offset).Limit(size).Find(&users).Error; err != nil {
		return nil, 0, err
	}
	return users, total, nil
}
