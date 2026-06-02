package store

import (
	"mail_go/internal/db"

	"gorm.io/gorm"
)

// DomainStore defines the interface for domain data operations.
type DomainStore interface {
	Create(domain *db.Domain) error
	GetByID(id uint) (*db.Domain, error)
	GetByName(name string) (*db.Domain, error)
	GetFirstTLSEnabledWithCert() (*db.Domain, error)
	Update(domain *db.Domain) error
	Delete(id uint) error
	List(page, size int) ([]db.Domain, int64, error)
}

// domainStoreGorm implements DomainStore using GORM.
type domainStoreGorm struct {
	db *gorm.DB
}

// newDomainStore creates a new GORM-backed DomainStore.
func newDomainStore(database *gorm.DB) DomainStore {
	return &domainStoreGorm{db: database}
}

// Create inserts a new domain record.
func (s *domainStoreGorm) Create(domain *db.Domain) error {
	return s.db.Create(domain).Error
}

// GetByID retrieves a domain by primary key.
func (s *domainStoreGorm) GetByID(id uint) (*db.Domain, error) {
	var domain db.Domain
	if err := s.db.First(&domain, id).Error; err != nil {
		return nil, err
	}
	return &domain, nil
}

// GetByName retrieves a domain by its name.
func (s *domainStoreGorm) GetByName(name string) (*db.Domain, error) {
	var domain db.Domain
	if err := s.db.Where("name = ?", name).First(&domain).Error; err != nil {
		return nil, err
	}
	return &domain, nil
}

// GetFirstTLSEnabledWithCert retrieves the first TLS-enabled domain with certificate paths.
func (s *domainStoreGorm) GetFirstTLSEnabledWithCert() (*db.Domain, error) {
	var domain db.Domain
	if err := s.db.Where("tls_enabled = ? AND tls_cert_path <> ? AND tls_key_path <> ?", true, "", "").Order("id ASC").First(&domain).Error; err != nil {
		return nil, err
	}
	return &domain, nil
}

// Update saves changes to an existing domain record.
func (s *domainStoreGorm) Update(domain *db.Domain) error {
	return s.db.Save(domain).Error
}

// Delete removes a domain by ID.
func (s *domainStoreGorm) Delete(id uint) error {
	return s.db.Delete(&db.Domain{}, id).Error
}

// List retrieves a paginated list of domains.
func (s *domainStoreGorm) List(page, size int) ([]db.Domain, int64, error) {
	var domains []db.Domain
	var total int64

	if err := s.db.Model(&db.Domain{}).Count(&total).Error; err != nil {
		return nil, 0, err
	}

	offset := (page - 1) * size
	if err := s.db.Offset(offset).Limit(size).Find(&domains).Error; err != nil {
		return nil, 0, err
	}
	return domains, total, nil
}
