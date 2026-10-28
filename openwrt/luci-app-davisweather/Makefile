#
# Copyright (C) 2026 meteogargano
#
# This is free software, licensed under the MIT License.
#

include $(TOPDIR)/rules.mk

PKG_NAME:=luci-app-davisweather
PKG_VERSION:=0.6
PKG_RELEASE:=1

PKG_LICENSE:=MIT
PKG_MAINTAINER:=meteogargano <meteogargano@users.noreply.github.com>

include $(INCLUDE_DIR)/package.mk

define Package/luci-app-davisweather
  SECTION:=luci
  CATEGORY:=LuCI
  SUBMENU:=3. Applications
  TITLE:=LuCI support for davisweather
  DEPENDS:=+luci-base +davisweather
  PKGARCH:=all
endef

define Package/luci-app-davisweather/description
  LuCI configuration page for the davisweather daemon: serial port,
  Voria2 API credentials, poll interval, rain bucket size and offline
  buffering mode. Save & Apply restarts the service.
endef

define Build/Prepare
endef

define Build/Configure
endef

define Build/Compile
endef

define Package/luci-app-davisweather/install
	$(INSTALL_DIR) $(1)/www/luci-static/resources/view/davisweather
	$(INSTALL_DATA) $(CURDIR)/htdocs/luci-static/resources/view/davisweather/*.js \
		$(1)/www/luci-static/resources/view/davisweather/

	$(INSTALL_DIR) $(1)/usr/share/luci/menu.d
	$(INSTALL_DATA) $(CURDIR)/root/usr/share/luci/menu.d/*.json \
		$(1)/usr/share/luci/menu.d/

	$(INSTALL_DIR) $(1)/usr/share/rpcd/acl.d
	$(INSTALL_DATA) $(CURDIR)/root/usr/share/rpcd/acl.d/*.json \
		$(1)/usr/share/rpcd/acl.d/
endef

$(eval $(call BuildPackage,luci-app-davisweather))
