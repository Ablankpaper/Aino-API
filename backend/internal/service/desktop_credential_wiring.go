package service

func ProvideDesktopAwareUserService(userRepo UserRepository, settingRepo SettingRepository, invalidator APIKeyAuthCacheInvalidator, billingCache BillingCache, desktopRepo DesktopCredentialRepository) *UserService {
	s := NewUserService(userRepo, settingRepo, invalidator, billingCache)
	s.SetDesktopCredentialRevoker(desktopRepo)
	return s
}
