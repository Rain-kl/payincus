# Layout 3-Tier Structure, 32px Footer & Brand Copyright Configuration Design

## 1. Overview
This design implements a classic 3-tier desktop/web layout for PayIncus / Incudal:
- **Top Region (Header)**: 56px (`h-14`) full-width top navigation bar.
- **Middle Region (Body)**: Flexible height (`flex-1 min-h-0 min-w-0`), divided horizontally into Left (SideNav) and Right (Main Content workspace).
- **Bottom Region (Footer)**: 32px (`h-8` / `h-[32px]`) full-width status footer, presenting:
  1. Copyright statement (`版权所有 © 2026， Arctel 和/或其关联公司。保留所有权利。`, dynamic from brand config).
  2. Terms of service and privacy notice link (`[使用条款和隐私声明]`, opens `TermsOfServiceModal`).
  3. Contact info (configured email `footerContactEmail` and Telegram `footerTelegramLink`).
- **Control Panel Configuration**: Add a "Copyright" (`brand_copyright`) configuration field under the Admin System Config -> Brand Settings section, allowing administrators to customize the copyright text with real-time updates.

---

## 2. Architecture & Data Flow

### 2.1 Backend Configuration & API
1. **Database / Seeds (`server/src/db/system-config.ts`)**:
   - Key: `brand_copyright`
   - Default value: `'版权所有 © 2026， Arctel 和/或其关联公司。保留所有权利。'`
   - Label: `版权所有`
   - Description: `站点页脚展示的版权所有文案，留空则使用默认值`
2. **Public API (`server/src/routes/system-config.ts`)**:
   - `GET /public`: reads `brand_copyright` and returns `brandCopyright: brandCopyright?.trim() || '版权所有 © 2026， Arctel 和/或其关联公司。保留所有权利。'`.
3. **Admin Config API (`server/src/routes/system-config.ts`)**:
   - `PUT /`: includes `'brand_copyright'` in `validKeys` and `stringKeys` with max length 500 validation.

### 2.2 Client State & Store
1. **Config Store (`client/src/stores/config.ts`)**:
   - Adds `brandCopyright = ref('版权所有 © 2026， Arctel 和/或其关联公司。保留所有权利。')`.
   - In `loadPublicConfig()`, updates `brandCopyright.value = config.brandCopyright?.trim() || '版权所有 © 2026， Arctel 和/或其关联公司。保留所有权利。'`.
   - Exports `brandCopyright`.
2. **Composable (`client/src/composables/useBrand.ts`)**:
   - Adds `get brandCopyright() { return configStore.brandCopyright?.trim() || '版权所有 © 2026， Arctel 和/或其关联公司。保留所有权利。' }`.

### 2.3 Admin Console UI (`client/src/views/admin/SystemConfigView.vue`)
1. **Brand Settings Group**:
   - `brandKeys` includes `'brand_copyright'`.
   - `form` includes `brand_copyright: ''`.
   - `stringConfigKeys` includes `'brand_copyright'`.
   - In the Brand Settings card, add a text input for Copyright (`版权所有`).
2. **i18n Messages (`client/src/locales/zh-CN.ts`, `zh-TW.ts`, `en.ts`)**:
   - `admin.system.brand.copyright`: '版权所有' / '版權所有' / 'Copyright Notice'
   - `admin.system.brand.copyrightDesc`: '显示在页面底部的版权所有文案，留空则使用默认值。' / '顯示在頁面底部的版權所有文案，留空則使用預設值。' / 'Copyright statement displayed at the bottom of the page. Leaves empty for default.'

---

## 3. Layout Specification (`AppLayout.vue`)

```
+------------------------------------------------------------------------------------+
|  Top Region: Header (h-14 / 56px, Full Width 100%)                                 |
|  [Logo & Brand]  [Collapse Toggle]             [Notifications] [User Avatar/Menu]  |
+------------------------------------------------------------------------------------+
|  Middle Region: (flex-1 flex min-h-0 min-w-0 overflow-hidden)                     |
|  +--------------------+---------------------------------------------------------+  |
|  | Left: SideNav      | Right: Main Content Workspace (overflow-auto)           |  |
|  | (w-60 / w-16)      |                                                         |  |
|  | Navigation Links   | [Dashboard / Instances / Billing / Settings / ...]      |  |
|  |                    |                                                         |  |
|  +--------------------+---------------------------------------------------------+  |
+------------------------------------------------------------------------------------+
|  Bottom Region: Footer (h-8 / 32px, Full Width 100%, shrink-0, border-t)           |
|  [版权所有 © 2026， Arctel 和/或其关联公司。保留所有权利。]   [使用条款和隐私声明] | [联系方式]  |
+------------------------------------------------------------------------------------+
```

### Footer Design Details:
- **Height**: Exactly `h-8` (`32px`), non-shrinking (`shrink-0`).
- **Visuals**:
  - Dark mode: `bg-[#1f1d1b] border-[#2c2a28] text-themed-muted`.
  - Light mode: `bg-[#f9f9f8] border-[#e0dfdd] text-themed-secondary`.
  - Border: `border-t`.
  - Text size: `text-xs` (11px-12px) with `font-normal`.
- **Left**: Copyright text `brand.brandCopyright`.
- **Right**:
  - `[使用条款和隐私声明]`: Click handler triggers `showTermsModal = true`.
  - Separator: `|` subtle line.
  - Contacts: `footerContactEmail` (with mailto and envelope icon) + `footerTelegramLink` (with external link and TG icon).
- **Modal Component**: `<TermsOfServiceModal :show="showTermsModal" @close="showTermsModal = false" />` integrated cleanly.

---

## 4. Verification Plan

1. **Automated Guard Tests**:
   - `pnpm --filter server test:frontend-route-guards`
   - `pnpm --filter server test:frontend-dist-boundary-guards`
   - `pnpm --filter server test:frontend-i18n-keys`
2. **Type-Check**:
   - `pnpm --filter client type-check`
   - `pnpm --filter server type-check`
3. **Build Verification**:
   - `pnpm build:client` (dual-target user and admin SPA build)
4. **Functional Testing**:
   - Verify footer renders properly at 32px height at the bottom of `AppLayout`.
   - Verify clicking `使用条款和隐私声明` opens `TermsOfServiceModal`.
   - Verify modifying `brand_copyright` in Admin System Settings reflects in the footer.
