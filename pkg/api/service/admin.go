package service

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/runestack/rune/pkg/api/generated"
	"github.com/runestack/rune/pkg/log"
	"github.com/runestack/rune/pkg/runner/docker/registryauth"
	"github.com/runestack/rune/pkg/store"
	"github.com/runestack/rune/pkg/store/repos"
	"github.com/runestack/rune/pkg/types"
	"github.com/runestack/rune/pkg/utils"
	"github.com/spf13/viper"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// AdminService implements generated.AdminServiceServer
type AdminService struct {
	generated.UnimplementedAdminServiceServer
	st     store.Store
	logger log.Logger
}

func NewAdminService(st store.Store, logger log.Logger) *AdminService {
	if logger == nil {
		logger = log.GetDefaultLogger().WithComponent("admin-service")
	} else {
		logger = logger.WithComponent("admin-service")
	}
	return &AdminService{st: st, logger: logger}
}

// AdminBootstrap creates the built-in root subject and returns a management token (opaque secret once)
func (s *AdminService) AdminBootstrap(ctx context.Context, _ *generated.AdminBootstrapRequest) (*generated.AdminBootstrapResponse, error) {
	// Ensure root user exists; create if missing
	ur := repos.NewUserRepo(s.st)
	root, err := ur.GetByName(ctx, "root")
	if store.IsNotFoundError(err) {
		createdRoot, err := ur.Create(ctx, &types.User{Name: "root"})
		if err != nil {
			return nil, err
		}
		root = createdRoot
	}

	// Ensure root policy attached to root user
	if err := s.ensureUserHasPolicy(ctx, ur, root, "root"); err != nil {
		return nil, err
	}
	// Issue root token with unique name
	tr := repos.NewTokenRepo(s.st)
	name := fmt.Sprintf("bootstrap-admin-%d", time.Now().UnixNano())
	tok, secret, err := tr.Issue(ctx, name, root.ID, "user", "bootstrap-admin", 0)
	if err != nil {
		return nil, err
	}
	return &generated.AdminBootstrapResponse{TokenId: tok.ID, TokenSecret: secret, SubjectId: root.ID}, nil
}

// ensureUserHasPolicy attaches policyName to user if missing
func (s *AdminService) ensureUserHasPolicy(ctx context.Context, ur *repos.UserRepo, u *types.User, policyName string) error {
	for _, p := range u.Policies {
		if p == policyName {
			return nil
		}
	}
	u.Policies = append(u.Policies, policyName)
	return ur.Update(ctx, u)
}

// PolicyCreate creates or updates a policy
func (s *AdminService) PolicyCreate(ctx context.Context, req *generated.PolicyCreateRequest) (*generated.PolicyCreateResponse, error) {
	pr := repos.NewPolicyRepo(s.st)
	p := &types.Policy{Name: req.Policy.Name, Description: req.Policy.Description, Builtin: false}
	for _, r := range req.Policy.Rules {
		p.Rules = append(p.Rules, types.PolicyRule{Resource: r.Resource, Verbs: r.Verbs, Namespace: r.Namespace})
	}
	if err := pr.Create(ctx, p); err != nil {
		// try update if exists
		if err := pr.Update(ctx, p); err != nil {
			return nil, err
		}
	}
	return &generated.PolicyCreateResponse{Policy: req.Policy}, nil
}

// PolicyUpdate updates a policy
func (s *AdminService) PolicyUpdate(ctx context.Context, req *generated.PolicyUpdateRequest) (*generated.PolicyUpdateResponse, error) {
	pr := repos.NewPolicyRepo(s.st)
	p := &types.Policy{Name: req.Policy.Name, Description: req.Policy.Description, Builtin: req.Policy.Builtin}
	for _, r := range req.Policy.Rules {
		p.Rules = append(p.Rules, types.PolicyRule{Resource: r.Resource, Verbs: r.Verbs, Namespace: r.Namespace})
	}
	if err := pr.Update(ctx, p); err != nil {
		return nil, err
	}
	return &generated.PolicyUpdateResponse{Policy: req.Policy}, nil
}

// PolicyDelete deletes a policy
func (s *AdminService) PolicyDelete(ctx context.Context, req *generated.PolicyDeleteRequest) (*generated.PolicyDeleteResponse, error) {
	pr := repos.NewPolicyRepo(s.st)
	name := req.Name
	if name == "" && req.Id != "" {
		name = req.Id
	}
	if err := pr.Delete(ctx, name); err != nil {
		return nil, err
	}
	return &generated.PolicyDeleteResponse{Deleted: true}, nil
}

// PolicyGet fetches a policy
func (s *AdminService) PolicyGet(ctx context.Context, req *generated.PolicyGetRequest) (*generated.PolicyGetResponse, error) {
	pr := repos.NewPolicyRepo(s.st)
	name := req.Name
	if name == "" && req.Id != "" {
		name = req.Id
	}
	p, err := pr.Get(ctx, name)
	if err != nil {
		return nil, err
	}
	gp := &generated.Policy{Id: p.ID, Name: p.Name, Description: p.Description, Builtin: p.Builtin}
	for _, r := range p.Rules {
		gp.Rules = append(gp.Rules, &generated.PolicyRule{Resource: r.Resource, Verbs: r.Verbs, Namespace: r.Namespace})
	}
	return &generated.PolicyGetResponse{Policy: gp}, nil
}

// PolicyList lists policies
func (s *AdminService) PolicyList(ctx context.Context, _ *generated.PolicyListRequest) (*generated.PolicyListResponse, error) {
	pr := repos.NewPolicyRepo(s.st)
	ps, err := pr.List(ctx)
	if err != nil {
		return nil, err
	}
	var out []*generated.Policy
	for _, p := range ps {
		gp := &generated.Policy{Id: p.ID, Name: p.Name, Description: p.Description, Builtin: p.Builtin}
		for _, r := range p.Rules {
			gp.Rules = append(gp.Rules, &generated.PolicyRule{Resource: r.Resource, Verbs: r.Verbs, Namespace: r.Namespace})
		}
		out = append(out, gp)
	}
	return &generated.PolicyListResponse{Policies: out}, nil
}

// TokenList lists tokens (admin-only). Does not return secrets.
func (s *AdminService) TokenList(ctx context.Context, _ *generated.TokenListRequest) (*generated.TokenListResponse, error) {
	tr := repos.NewTokenRepo(s.st)
	list, err := tr.List(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]*generated.TokenInfo, 0, len(list))
	for _, t := range list {
		ti := &generated.TokenInfo{
			Id:          t.ID,
			Name:        t.Name,
			SubjectId:   t.SubjectID,
			SubjectType: t.SubjectType,
			Description: t.Description,
			IssuedAt:    t.IssuedAt.Unix(),
			Revoked:     t.Revoked,
		}
		if t.ExpiresAt != nil {
			ti.ExpiresAt = t.ExpiresAt.Unix()
		}
		out = append(out, ti)
	}
	return &generated.TokenListResponse{Tokens: out}, nil
}

// Registry admin RPCs
func (s *AdminService) ListRegistries(ctx context.Context, _ *generated.ListRegistriesRequest) (*generated.ListRegistriesResponse, error) {
	var out []*generated.RegistryConfig
	if viper.IsSet("docker.registries") {
		var regs []map[string]any
		if err := viper.UnmarshalKey("docker.registries", &regs); err == nil {
			for _, r := range regs {
				rc := buildRegistryFromMap(r)
				out = append(out, rc)
			}
		}
	}
	return &generated.ListRegistriesResponse{Registries: out}, nil
}

func buildRegistryFromMap(r map[string]any) *generated.RegistryConfig {
	rc := &generated.RegistryConfig{}
	if name, ok := r["name"].(string); ok {
		rc.Name = name
	}
	if reg, ok := r["registry"].(string); ok {
		rc.Registry = reg
	}
	if authMap, ok := r["auth"].(map[string]any); ok {
		a := &generated.RegistryAuthConfig{Data: map[string]string{}}
		if v, ok := authMap["type"].(string); ok {
			a.Type = v
		}
		if v, ok := authMap["username"].(string); ok {
			a.Username = v
		}
		if v, ok := authMap["password"].(string); ok {
			a.Password = v
		}
		if v, ok := authMap["token"].(string); ok {
			a.Token = v
		}
		if v, ok := authMap["region"].(string); ok {
			a.Region = v
		}
		if v, ok := authMap["fromSecret"].(string); ok {
			a.FromSecret = v
		}
		if v, ok := authMap["from_secret"].(string); ok {
			a.FromSecret = v
		}
		if v, ok := authMap["bootstrap"].(bool); ok {
			a.Bootstrap = v
		}
		if v, ok := authMap["manage"].(string); ok {
			a.Manage = v
		}
		if v, ok := authMap["immutable"].(bool); ok {
			a.Immutable = v
		}
		if dm, ok := authMap["data"].(map[string]any); ok {
			for k, val := range dm {
				if sv, ok := val.(string); ok {
					a.Data[k] = sv
				}
			}
		}
		rc.Auth = a
	}
	return rc
}

func asString(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

func (s *AdminService) GetRegistry(ctx context.Context, req *generated.GetRegistryRequest) (*generated.GetRegistryResponse, error) {
	name := req.GetName()
	if name == "" {
		return nil, status.Error(codes.InvalidArgument, "name is required")
	}
	var regs []map[string]any
	_ = viper.UnmarshalKey("docker.registries", &regs)
	for _, r := range regs {
		if rname, ok := r["name"].(string); ok && rname == name {
			rc := buildRegistryFromMap(r)
			return &generated.GetRegistryResponse{Registry: rc}, nil
		}
	}
	return nil, status.Error(codes.NotFound, "registry not found")
}

func normalizeAuthMap(a map[string]any) map[string]any {
	if a == nil {
		return nil
	}
	// harmonize key spelling
	if v, ok := a["from_secret"]; ok {
		if _, exists := a["fromSecret"]; !exists {
			a["fromSecret"] = v
		}
		delete(a, "from_secret")
	}
	// infer type if missing
	if _, ok := a["type"]; !ok || a["type"] == "" {
		if tok, ok := a["token"].(string); ok && tok != "" {
			a["type"] = "token"
		} else if _, ok := a["username"].(string); ok {
			if _, ok := a["password"].(string); ok {
				a["type"] = "basic"
			}
		} else if reg, ok := a["region"].(string); ok && reg != "" {
			a["type"] = "ecr"
		}
	}
	return a
}

func (s *AdminService) AddRegistry(ctx context.Context, req *generated.AddRegistryRequest) (*generated.AddRegistryResponse, error) {
	rc := req.GetRegistry()
	if rc == nil || rc.GetName() == "" || rc.GetRegistry() == "" {
		return nil, status.Error(codes.InvalidArgument, "name and registry are required")
	}
	var regs []map[string]any
	_ = viper.UnmarshalKey("docker.registries", &regs)
	for _, r := range regs {
		if rname, ok := r["name"].(string); ok && rname == rc.GetName() {
			return nil, status.Error(codes.AlreadyExists, "registry name already exists")
		}
	}
	entry := map[string]any{"name": rc.GetName(), "registry": rc.GetRegistry()}
	if rc.Auth != nil {
		auth := map[string]any{}
		if v := rc.Auth.GetType(); v != "" {
			auth["type"] = v
		}
		if v := rc.Auth.GetUsername(); v != "" {
			auth["username"] = v
		}
		if v := rc.Auth.GetPassword(); v != "" {
			auth["password"] = v
		}
		if v := rc.Auth.GetToken(); v != "" {
			auth["token"] = v
		}
		if v := rc.Auth.GetRegion(); v != "" {
			auth["region"] = v
		}
		if v := rc.Auth.GetFromSecret(); v != "" {
			auth["fromSecret"] = v
		}
		if rc.Auth.GetBootstrap() {
			auth["bootstrap"] = true
		}
		if v := rc.Auth.GetManage(); v != "" {
			auth["manage"] = v
		}
		if rc.Auth.GetImmutable() {
			auth["immutable"] = true
		}
		if len(rc.Auth.GetData()) > 0 {
			auth["data"] = rc.Auth.GetData()
		}
		entry["auth"] = normalizeAuthMap(auth)
	}
	regs = append(regs, entry)
	viper.Set("docker.registries", regs)
	if err := viper.WriteConfig(); err != nil {
		s.logger.Warn("Failed to write runefile config", log.Err(err))
	}
	return &generated.AddRegistryResponse{Registry: rc}, nil
}

func (s *AdminService) UpdateRegistry(ctx context.Context, req *generated.UpdateRegistryRequest) (*generated.UpdateRegistryResponse, error) {
	rc := req.GetRegistry()
	if rc == nil || rc.GetName() == "" {
		return nil, status.Error(codes.InvalidArgument, "name is required")
	}
	var regs []map[string]any
	_ = viper.UnmarshalKey("docker.registries", &regs)
	updated := false
	for i, r := range regs {
		if rname, ok := r["name"].(string); ok && rname == rc.GetName() {
			entry := map[string]any{"name": rc.GetName(), "registry": utils.PickFirstNonEmpty(rc.GetRegistry(), asString(r["registry"]))}
			if rc.Auth != nil {
				auth := map[string]any{}
				if v := rc.Auth.GetType(); v != "" {
					auth["type"] = v
				}
				if v := rc.Auth.GetUsername(); v != "" {
					auth["username"] = v
				}
				if v := rc.Auth.GetPassword(); v != "" {
					auth["password"] = v
				}
				if v := rc.Auth.GetToken(); v != "" {
					auth["token"] = v
				}
				if v := rc.Auth.GetRegion(); v != "" {
					auth["region"] = v
				}
				if v := rc.Auth.GetFromSecret(); v != "" {
					auth["fromSecret"] = v
				}
				if rc.Auth.GetBootstrap() {
					auth["bootstrap"] = true
				}
				if v := rc.Auth.GetManage(); v != "" {
					auth["manage"] = v
				}
				if rc.Auth.GetImmutable() {
					auth["immutable"] = true
				}
				if len(rc.Auth.GetData()) > 0 {
					auth["data"] = rc.Auth.GetData()
				}
				entry["auth"] = normalizeAuthMap(auth)
			} else if a, ok := r["auth"].(map[string]any); ok {
				entry["auth"] = normalizeAuthMap(a)
			}
			regs[i] = entry
			updated = true
			break
		}
	}
	if !updated {
		return nil, status.Error(codes.NotFound, "registry not found")
	}
	viper.Set("docker.registries", regs)
	if err := viper.WriteConfig(); err != nil {
		s.logger.Warn("Failed to write runefile config", log.Err(err))
	}
	return &generated.UpdateRegistryResponse{Registry: rc}, nil
}

func (s *AdminService) RemoveRegistry(ctx context.Context, req *generated.RemoveRegistryRequest) (*generated.RemoveRegistryResponse, error) {
	name := req.GetName()
	if name == "" {
		return nil, status.Error(codes.InvalidArgument, "name is required")
	}
	var regs []map[string]any
	_ = viper.UnmarshalKey("docker.registries", &regs)
	var out []map[string]any
	deleted := false
	for _, r := range regs {
		if rname, ok := r["name"].(string); ok && rname == name {
			deleted = true
			continue
		}
		out = append(out, r)
	}
	if !deleted {
		return nil, status.Error(codes.NotFound, "registry not found")
	}
	viper.Set("docker.registries", out)
	if err := viper.WriteConfig(); err != nil {
		s.logger.Warn("Failed to write runefile config", log.Err(err))
	}
	return &generated.RemoveRegistryResponse{Deleted: true}, nil
}

func (s *AdminService) BootstrapAuth(ctx context.Context, req *generated.BootstrapAuthRequest) (*generated.BootstrapAuthResponse, error) {
	var regs []map[string]any
	_ = viper.UnmarshalKey("docker.registries", &regs)
	if len(regs) == 0 {
		return &generated.BootstrapAuthResponse{Updated: 0}, nil
	}
	nameFilter := req.GetName()
	typeFilter := req.GetType()
	all := req.GetAll()
	updated := int32(0)

	secRepo := repos.NewSecretRepo(s.st)
	var outRegs []map[string]any
	for _, r := range regs {
		selected := all
		if !selected && nameFilter != "" {
			if n, ok := r["name"].(string); ok && n == nameFilter {
				selected = true
			}
		}
		if !selected && typeFilter != "" {
			if a, ok := r["auth"].(map[string]any); ok {
				if t, ok := a["type"].(string); ok && t == typeFilter {
					selected = true
				}
			}
		}

		if !selected {
			// keep as-is
			outRegs = append(outRegs, r)
			continue
		}

		entry := map[string]any{
			"name":     r["name"],
			"registry": r["registry"],
		}
		authOut := map[string]any{}
		if a, ok := r["auth"].(map[string]any); ok {
			// copy through simple fields
			if v, ok := a["type"].(string); ok {
				authOut["type"] = v
			}
			if v, ok := a["region"].(string); ok {
				authOut["region"] = v
			}
			// resolve fromSecret, or fall back to direct fields
			fromSecret := ""
			if v, ok := a["fromSecret"].(string); ok {
				fromSecret = v
			}
			if v, ok := a["from_secret"].(string); ok {
				fromSecret = v
			}
			if fromSecret != "" {
				// Bootstrap if requested
				bootstrap := false
				if v, ok := a["bootstrap"].(bool); ok {
					bootstrap = v
				}
				immutable := false
				if v, ok := a["immutable"].(bool); ok {
					immutable = v
				}
				manage := ""
				if v, ok := a["manage"].(string); ok {
					manage = v
				}
				data := map[string]string{}
				if dm, ok := a["data"].(map[string]any); ok {
					for k, val := range dm {
						if sv, ok := val.(string); ok {
							data[k] = os.ExpandEnv(sv)
						}
					}
				}
				ns := "system"
				nameOnly := fromSecret
				// basic parsing for ns/name if provided as ns/name
				if i := strings.IndexByte(fromSecret, '/'); i > 0 {
					ns = fromSecret[:i]
					nameOnly = fromSecret[i+1:]
				}
				if bootstrap && len(data) > 0 {
					_ = s.bootstrapRegistrySecret(ctx, secRepo, ns, nameOnly, data, immutable, manage)
				}
				// resolve into authOut
				_ = s.resolveRegistrySecret(ctx, secRepo, ns, nameOnly, authOut)
				// ensure region kept
				if v, ok := a["region"].(string); ok {
					authOut["region"] = v
				}
			} else {
				// Direct fields with env expansion
				if v, ok := a["username"].(string); ok {
					authOut["username"] = os.ExpandEnv(v)
				}
				if v, ok := a["password"].(string); ok {
					authOut["password"] = os.ExpandEnv(v)
				}
				if v, ok := a["token"].(string); ok {
					authOut["token"] = os.ExpandEnv(v)
				}
				if _, ok := authOut["type"]; !ok {
					if _, ok := authOut["token"].(string); ok {
						authOut["type"] = "token"
					} else if _, uok := authOut["username"].(string); uok {
						if _, pok := authOut["password"].(string); pok {
							authOut["type"] = "basic"
						}
					}
				}
			}
		}
		entry["auth"] = authOut
		outRegs = append(outRegs, entry)
		updated++
	}

	viper.Set("docker.registries", outRegs)
	if err := viper.WriteConfig(); err != nil {
		s.logger.Warn("Failed to write runefile config", log.Err(err))
	}
	return &generated.BootstrapAuthResponse{Updated: updated}, nil
}

func (s *AdminService) resolveRegistrySecret(ctx context.Context, secRepo *repos.SecretRepo, ns, name string, auth map[string]any) error {
	sc, err := secRepo.Get(ctx, ns, name)
	if err != nil {
		return err
	}
	if val, ok := sc.Data[".dockerconfigjson"]; ok && val != "" {
		auth["type"] = "dockerconfigjson"
		auth["dockerconfigjson"] = val
		return nil
	}
	if tk, ok := sc.Data["token"]; ok && tk != "" {
		auth["type"] = "token"
		auth["token"] = tk
		return nil
	}
	if tk, ok := sc.Data["tok"]; ok && tk != "" {
		auth["type"] = "token"
		auth["tok"] = tk
		return nil
	}
	if u, uok := sc.Data["username"]; uok {
		if p, pok := sc.Data["password"]; pok {
			auth["type"] = "basic"
			auth["username"] = u
			auth["password"] = p
			return nil
		}
	}
	if u, uok := sc.Data["user"]; uok {
		if p, pok := sc.Data["pass"]; pok {
			auth["type"] = "basic"
			auth["user"] = u
			auth["pass"] = p
			return nil
		}
	}
	if ak, ok := sc.Data["awsAccessKeyId"]; ok {
		auth["type"] = "ecr"
		auth["awsAccessKeyId"] = ak
		if sk, ok := sc.Data["awsSecretAccessKey"]; ok {
			auth["awsSecretAccessKey"] = sk
		}
		if st, ok := sc.Data["awsSessionToken"]; ok {
			auth["awsSessionToken"] = st
		}
		return nil
	}
	return nil
}

func (s *AdminService) bootstrapRegistrySecret(ctx context.Context, secRepo *repos.SecretRepo, ns, name string, data map[string]string, immutable bool, manage string) error {
	existing, err := secRepo.Get(ctx, ns, name)
	if err != nil {
		// create
		sec := &types.Secret{Name: name, Namespace: ns, Data: data, Type: "static"}
		return secRepo.Create(ctx, sec)
	}
	if immutable {
		return nil
	}
	if manage == "ignore" {
		return nil
	}
	sec := &types.Secret{Name: name, Namespace: ns, Data: data, Type: existing.Type}
	return secRepo.Update(ctx, ns, name, sec)
}

func (s *AdminService) TestRegistry(ctx context.Context, req *generated.TestRegistryRequest) (*generated.TestRegistryResponse, error) {
	name := req.GetName()
	if name == "" {
		return nil, status.Error(codes.InvalidArgument, "name is required")
	}
	// Build a single-entry provider config from current viper
	var regs []map[string]any
	_ = viper.UnmarshalKey("docker.registries", &regs)
	var target map[string]any
	for _, r := range regs {
		if rname, ok := r["name"].(string); ok && rname == name {
			target = r
			break
		}
	}
	if target == nil {
		return nil, status.Error(codes.NotFound, "registry not found")
	}
	providers := registryauth.BuildProviders(ctx, []map[string]any{target})
	if len(providers) == 0 {
		return &generated.TestRegistryResponse{Ok: false, Message: "no provider for registry"}, nil
	}
	// For now, just attempt to resolve auth; providers that need network may do so internally.
	p := providers[0]
	if _, err := p.Resolve(ctx, "", req.GetImageRef()); err != nil {
		return &generated.TestRegistryResponse{Ok: false, Message: err.Error()}, nil
	}
	return &generated.TestRegistryResponse{Ok: true, Message: "ok"}, nil
}

func (s *AdminService) RegistriesStatus(ctx context.Context, _ *generated.RegistriesStatusRequest) (*generated.RegistriesStatusResponse, error) {
	var regs []map[string]any
	_ = viper.UnmarshalKey("docker.registries", &regs)
	var out []*generated.RegistryRuntimeStatus
	for _, r := range regs {
		name := asString(r["name"])
		reg := asString(r["registry"])
		typ := ""
		authConfigured := false
		if a, ok := r["auth"].(map[string]any); ok {
			a = normalizeAuthMap(a)
			if t, ok := a["type"].(string); ok {
				typ = t
			}
			if typ != "" {
				authConfigured = true
			}
			if !authConfigured {
				if _, ok := a["token"].(string); ok {
					authConfigured = true
				}
				if _, ok := a["username"].(string); ok {
					authConfigured = true
				}
				if _, ok := a["dockerconfigjson"].(string); ok {
					authConfigured = true
				}
			}
		}
		out = append(out, &generated.RegistryRuntimeStatus{
			Name:            name,
			Registry:        reg,
			Type:            typ,
			AuthConfigured:  authConfigured,
			LastBootstrapAt: "",
			LastError:       "",
		})
	}
	return &generated.RegistriesStatusResponse{Registries: out}, nil
}

// PolicyAttachToSubject attaches a policy to a user subject (MVP: users only)
func (s *AdminService) PolicyAttachToSubject(ctx context.Context, req *generated.PolicyAttachToSubjectRequest) (*generated.PolicyAttachToSubjectResponse, error) {
	ur := repos.NewUserRepo(s.st)
	u, err := ur.GetByNameOrID(ctx, utils.PickFirstNonEmpty(req.SubjectId, req.SubjectName))
	if err != nil {
		return nil, err
	}
	pol := req.PolicyName
	if pol == "" && req.PolicyId != "" {
		pol = req.PolicyId
	}
	if err := s.ensureUserHasPolicy(ctx, ur, u, pol); err != nil {
		return nil, err
	}
	return &generated.PolicyAttachToSubjectResponse{Attached: true}, nil
}

// PolicyDetachFromSubject detaches a policy from a user subject (MVP: users only)
func (s *AdminService) PolicyDetachFromSubject(ctx context.Context, req *generated.PolicyDetachFromSubjectRequest) (*generated.PolicyDetachFromSubjectResponse, error) {
	ur := repos.NewUserRepo(s.st)
	u, err := ur.GetByNameOrID(ctx, utils.PickFirstNonEmpty(req.SubjectId, req.SubjectName))
	if err != nil {
		return nil, err
	}
	pol := req.PolicyName
	if pol == "" && req.PolicyId != "" {
		pol = req.PolicyId
	}
	var filtered []string
	for _, p := range u.Policies {
		if p != pol {
			filtered = append(filtered, p)
		}
	}
	u.Policies = filtered
	if err := ur.Update(ctx, u); err != nil {
		return nil, err
	}
	return &generated.PolicyDetachFromSubjectResponse{Detached: true}, nil
}

// UserCreate upserts a user and attaches policies (MVP)
func (s *AdminService) UserCreate(ctx context.Context, req *generated.UserCreateRequest) (*generated.UserCreateResponse, error) {
	ur := repos.NewUserRepo(s.st)
	// Try get by name; if not exists, create
	u, err := ur.GetByName(ctx, req.Name)
	if err != nil {
		u = &types.User{Name: req.Name, Email: req.Email}
		createdUser, err := ur.Create(ctx, u)
		if err != nil {
			return nil, err
		}
		u = createdUser
	} else {
		// update email if provided
		if req.Email != "" {
			u.Email = req.Email
		}
	}
	// attach policies (dedupe)
	seen := map[string]struct{}{}
	for _, p := range u.Policies {
		seen[p] = struct{}{}
	}
	for _, p := range req.Policies {
		seen[p] = struct{}{}
	}
	u.Policies = u.Policies[:0]
	for p := range seen {
		u.Policies = append(u.Policies, p)
	}
	if err := ur.Update(ctx, u); err != nil {
		return nil, err
	}
	return &generated.UserCreateResponse{User: &generated.Subject{Id: u.ID, Kind: "user", Name: u.Name, Email: u.Email, Policies: u.Policies}}, nil
}

// UserList lists users (subjects)
func (s *AdminService) UserList(ctx context.Context, _ *generated.UserListRequest) (*generated.UserListResponse, error) {
	var users []types.User
	if err := s.st.List(ctx, types.ResourceTypeUser, "system", &users); err != nil {
		return nil, err
	}
	var out []*generated.Subject
	for _, u := range users {
		uu := u
		out = append(out, &generated.Subject{Id: uu.ID, Kind: "user", Name: uu.Name, Email: uu.Email, Policies: uu.Policies})
	}
	return &generated.UserListResponse{Users: out}, nil
}
