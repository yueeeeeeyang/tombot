package service

import (
	"context"
	"database/sql"
	"time"

	"io.yueyang/tom/internal/browser"
	"io.yueyang/tom/internal/domain"
	"io.yueyang/tom/internal/policy"
	"io.yueyang/tom/internal/store"
)

const browserOriginLimit = 64

// CanVisit 供工具在创建审批前检查许可；执行时和每次文档请求仍会再次检查拒绝规则。
func (s *Browser) CanVisit(ctx context.Context, botID, tabID, address string) (bool, error) {
	origin, err := browser.Origin(address)
	if err != nil {
		return false, err
	}
	return browserOriginAllowed(ctx, s.db, botID, tabID, origin)
}

func browserOriginAllowed(ctx context.Context, reader policyReader, botID, tabID, origin string) (bool, error) {
	decision, err := readPolicyDecision(ctx, reader, botID, policy.Action{Class: "net.new_domain", Pattern: origin})
	if err != nil {
		return false, err
	}
	if decision == "deny" {
		return false, domain.NewError(403, "TOOL_DENIED", "A policy rule denied this browser origin.")
	}
	if decision == "allow" {
		return true, nil
	}
	var allowed bool
	err = reader.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM browser_origin_permissions p JOIN browser_tabs t ON t.id=p.tab_id WHERE p.tab_id=? AND t.bot_id=? AND p.origin=?)", tabID, botID, origin).Scan(&allowed)
	return allowed, err
}

// grantBrowserOrigin 只能在已批准的命令或用户交接事务中调用；自动跳转和弹窗不能自行扩张许可。
func grantBrowserOrigin(ctx context.Context, tx *sql.Tx, tab BrowserTab, address, actor string) error {
	origin, err := browser.Origin(address)
	if err != nil {
		return err
	}
	if _, err = browserOriginAllowed(ctx, tx, tab.BotID, tab.ID, origin); err != nil {
		return err
	}
	var count int
	var exists bool
	if err = tx.QueryRowContext(ctx, "SELECT COUNT(*),COALESCE(MAX(origin=?),0) FROM browser_origin_permissions WHERE tab_id=?", origin, tab.ID).Scan(&count, &exists); err != nil {
		return err
	}
	if exists {
		return nil
	}
	if count >= browserOriginLimit {
		return domain.NewError(409, "BROWSER_ORIGIN_LIMIT", "This tab reached its origin limit. Close it and open a new tab.")
	}
	if _, err = tx.ExecContext(ctx, "INSERT INTO browser_origin_permissions(tab_id,origin,created_at) VALUES(?,?,?)", tab.ID, origin, time.Now().UnixMilli()); err != nil {
		return err
	}
	return store.Audit(ctx, tx, actor, "browser.origin.granted", tab.ID, map[string]string{"bot_id": tab.BotID, "origin": origin})
}

// authorizeNavigation 由运行时请求线程调用，只访问数据库，不获取服务或浏览器命令锁。
func (s *Browser) authorizeNavigation(ctx context.Context, request browser.Navigation) error {
	tab, err := s.Get(ctx, request.TabID)
	if err != nil {
		return err
	}
	if tab.owner() != request.Owner {
		return domain.NewError(403, "BROWSER_TAB_FORBIDDEN", "The navigation owner changed.")
	}
	if tab.Status != "active" && tab.Status != "opening" && tab.Status != "acting" {
		return domain.NewError(409, "BROWSER_TAB_INACTIVE", "The tab is not active.")
	}
	if tab.Controller == "user" {
		return nil
	}
	origin, err := browser.Origin(request.URL)
	if err != nil {
		return err
	}
	allowed, decisionErr := browserOriginAllowed(ctx, s.db, tab.BotID, tab.ID, origin)
	if decisionErr == nil && allowed {
		return nil
	}
	code := "BROWSER_DOMAIN_APPROVAL_REQUIRED"
	if decisionErr != nil {
		code = browserFailure(decisionErr)
	}
	if err = store.Transact(ctx, s.db, func(tx *sql.Tx) error {
		return store.Audit(ctx, tx, "system", "browser.navigation.blocked", tab.ID, map[string]string{"bot_id": tab.BotID, "origin": origin, "method": request.Method, "error_code": code})
	}); err != nil {
		return err
	}
	if decisionErr != nil {
		return decisionErr
	}
	return domain.NewError(403, code, "This origin has not been approved for the tab. Request approval in a new call; never replay a form submission automatically.")
}

func blockedBrowserFields(blocked *browser.BlockedNavigation) (string, string, string) {
	if blocked == nil {
		return "", "", ""
	}
	return blocked.URL, blocked.Method, blocked.Code
}
func sameBlockedBrowserNavigation(left, right *browser.BlockedNavigation) bool {
	if left == nil || right == nil {
		return left == right
	}
	return *left == *right
}
