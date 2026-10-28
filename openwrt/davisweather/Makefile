#
# Copyright (C) 2026 meteogargano
#
# This is free software, licensed under the MIT License.
#

include $(TOPDIR)/rules.mk

PKG_NAME:=davisweather
PKG_VERSION:=0.6
PKG_RELEASE:=1

PKG_SOURCE:=$(PKG_NAME)-$(PKG_VERSION).tar.gz
PKG_SOURCE_URL:=https://codeload.github.com/meteogargano/daviscompanion/tar.gz/v$(PKG_VERSION)?
# No PKG_HASH: the feed Makefile lives inside the same repository it
# downloads, so a source hash would be self-referential. The buildroot only
# emits a "no hash" warning; the URL is fixed and served over HTTPS.
PKG_BUILD_DIR:=$(BUILD_DIR)/daviscompanion-$(PKG_VERSION)

PKG_MAINTAINER:=meteogargano <meteogargano@users.noreply.github.com>
PKG_LICENSE:=MIT
PKG_LICENSE_FILES:=LICENSE

PKG_BUILD_DEPENDS:=golang/host
PKG_BUILD_PARALLEL:=1
PKG_BUILD_FLAGS:=no-mips16

GO_PKG:=github.com/meteogargano/davisweather
GO_PKG_BUILD_PKG:=github.com/meteogargano/davisweather
GO_PKG_LDFLAGS_X:=main.version=$(PKG_VERSION)

include $(INCLUDE_DIR)/package.mk
# golang build helpers come from the official packages feed
include ../../packages/lang/golang/golang-package.mk

define Package/davisweather
  SECTION:=utils
  CATEGORY:=Utilities
  TITLE:=Davis Vantage Pro weather station uploader
  URL:=https://github.com/meteogargano/daviscompanion
  DEPENDS:=$(GO_ARCH_DEPENDS) +ca-bundle
  SUGGESTS:=kmod-usb-serial-ftdi kmod-usb-serial-pl2303 kmod-usb-serial-ch341 kmod-usb-serial-cp210x
endef

define Package/davisweather/description
  Reads a Davis Vantage Pro / Pro2 / Vue console over the WeatherLink
  serial interface and uploads measurements to the Voria2 Ingest API.
  Failed uploads can be buffered in SQLite and retried automatically
  (off by default: UCI buffering=none runs stateless via --no-db).
endef

define Package/davisweather/conffiles
/etc/config/davisweather
endef

define Package/davisweather/install
	$(call GoPackage/Package/Install/Bin,$(PKG_INSTALL_DIR))
	$(INSTALL_DIR) $(1)/usr/bin
	$(INSTALL_BIN) $(PKG_INSTALL_DIR)/usr/bin/davisweather $(1)/usr/bin/davisweather

	$(INSTALL_DIR) $(1)/etc/config
	$(INSTALL_CONF) $(CURDIR)/files/davisweather.conf $(1)/etc/config/davisweather

	$(INSTALL_DIR) $(1)/etc/init.d
	$(INSTALL_BIN) $(CURDIR)/files/davisweather.init $(1)/etc/init.d/davisweather
endef

$(eval $(call BuildPackage,davisweather))
