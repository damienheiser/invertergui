# OpenWRT Package Makefile for energymanager
# Target: GL.iNet GL-SFT1200 (Opal) - Siflower SF19A28 MIPS

include $(TOPDIR)/rules.mk

PKG_NAME:=energymanager
PKG_VERSION:=1.0.0
PKG_RELEASE:=1

PKG_BUILD_DIR:=$(BUILD_DIR)/$(PKG_NAME)-$(PKG_VERSION)
PKG_BUILD_DEPENDS:=golang/host

include $(INCLUDE_DIR)/package.mk
include $(TOPDIR)/feeds/packages/lang/golang/golang-package.mk

define Package/energymanager
  SECTION:=utils
  CATEGORY:=Utilities
  TITLE:=Energy Manager for Shelly Pro EM3 and Victron MultiPlus
  DEPENDS:=$(GO_ARCH_DEPENDS) +kmod-usb-serial-ftdi
  MAINTAINER:=Energy Manager <noreply@example.com>
endef

define Package/energymanager/description
  Energy management system that monitors grid power via Shelly Pro EM3
  (Modbus TCP) and controls a Victron MultiPlus inverter to maintain
  zero grid power. Includes web UI, Prometheus metrics, and MQTT.
endef

define Package/energymanager/conffiles
/etc/config/energymanager
endef

GO_PKG:=github.com/diebietse/invertergui/energymanager
GO_PKG_BUILD_PKG:=$(GO_PKG)/cmd/energymanager

define Build/Prepare
	mkdir -p $(PKG_BUILD_DIR)
	$(CP) ./src/* $(PKG_BUILD_DIR)/
endef

define Package/energymanager/install
	$(INSTALL_DIR) $(1)/usr/bin
	$(INSTALL_BIN) $(GO_PKG_BUILD_BIN_DIR)/energymanager $(1)/usr/bin/
	$(INSTALL_DIR) $(1)/etc/init.d
	$(INSTALL_BIN) ./files/energymanager.init $(1)/etc/init.d/energymanager
	$(INSTALL_DIR) $(1)/etc/config
	$(INSTALL_CONF) ./files/energymanager.config $(1)/etc/config/energymanager
endef

$(eval $(call GoBinPackage,energymanager))
$(eval $(call BuildPackage,energymanager))
