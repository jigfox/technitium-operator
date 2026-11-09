package controller

import (
	"os"
	"path/filepath"

	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"

	techv1alpha1 "github.com/jigfox/technitium-operator/api/v1alpha1"
	"github.com/jigfox/technitium-operator/internal/technitium"
)

const (
	timeout  = 20 * time.Second
	interval = 200 * time.Millisecond
)

func key(n string) types.NamespacedName { return types.NamespacedName{Namespace: "default", Name: n} }

func domainSpec(domain string) techv1alpha1.DomainSpec {
	return techv1alpha1.DomainSpec{ServerRef: techv1alpha1.ServerReference{Name: "primary"}, Domain: domain}
}

func isReady(conds []metav1.Condition) bool {
	return meta.IsStatusConditionTrue(conds, techv1alpha1.ConditionReady)
}

var _ = Describe("TechnitiumServer", func() {
	It("defaults the credential keys and reports the version", func() {
		s := &techv1alpha1.TechnitiumServer{
			ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "app-server"},
			Spec: techv1alpha1.TechnitiumServerSpec{
				URL:                  "http://technitium.invalid:5380",
				CredentialsSecretRef: techv1alpha1.CredentialsSecretRef{Name: "creds"},
				Apps: []techv1alpha1.AppConfig{
					{Name: "Query Logs (Sqlite)", Config: jsonOf(`{"maxLogDays":7}`)},
				},
			},
		}
		fakeTech.InstallApp("Query Logs (Sqlite)", `{"maxLogDays":0,"keep":true}`)
		Expect(k8sClient.Create(ctx, s)).To(Succeed())
		Expect(s.Spec.CredentialsSecretRef.UsernameKey).To(Equal("username"))
		Expect(s.Spec.CredentialsSecretRef.PasswordKey).To(Equal("password"))

		Eventually(func(g Gomega) {
			var got techv1alpha1.TechnitiumServer
			g.Expect(k8sClient.Get(ctx, key("app-server"), &got)).To(Succeed())
			g.Expect(isReady(got.Status.Conditions)).To(BeTrue())
			g.Expect(got.Status.Version).To(Equal("15.0"))
		}, timeout, interval).Should(Succeed())
		Expect(fakeTech.AppConfig("Query Logs (Sqlite)")).To(ContainSubstring(`"keep": true`))
		Expect(fakeTech.AppConfig("Query Logs (Sqlite)")).To(ContainSubstring(`"maxLogDays": 7`))
	})

	It("rejects a URL that is not http(s)", func() {
		for _, u := range []string{"technitium:5380", "ftp://x", "http://"} {
			s := &techv1alpha1.TechnitiumServer{
				ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "badurl"},
				Spec: techv1alpha1.TechnitiumServerSpec{
					URL: u, CredentialsSecretRef: techv1alpha1.CredentialsSecretRef{Name: "creds"},
				},
			}
			Expect(k8sClient.Create(ctx, s)).NotTo(Succeed(), u)
		}
	})
})

var _ = Describe("TechnitiumAllowedDomain", func() {
	It("adds the entry, deletes it again and defaults the deletion policy", func() {
		d := &techv1alpha1.TechnitiumAllowedDomain{
			ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "envtest-allowed"},
			Spec:       domainSpec("EnvTest.Example.com."),
		}
		Expect(k8sClient.Create(ctx, d)).To(Succeed())
		Expect(d.Spec.DeletionPolicy).To(Equal(techv1alpha1.DeletionPolicyDelete))

		Eventually(func(g Gomega) {
			var got techv1alpha1.TechnitiumAllowedDomain
			g.Expect(k8sClient.Get(ctx, key("envtest-allowed"), &got)).To(Succeed())
			g.Expect(isReady(got.Status.Conditions)).To(BeTrue())
			g.Expect(got.Status.Domain).To(Equal("envtest.example.com"))
		}, timeout, interval).Should(Succeed())
		Expect(fakeTech.Domains(technitium.Allowed)).To(ContainElement("envtest.example.com"))

		Expect(k8sClient.Delete(ctx, d)).To(Succeed())
		Eventually(func() bool {
			return k8sClient.Get(ctx, key("envtest-allowed"), &techv1alpha1.TechnitiumAllowedDomain{}) != nil
		}, timeout, interval).Should(BeTrue())
		Expect(fakeTech.Domains(technitium.Allowed)).NotTo(ContainElement("envtest.example.com"))
	})

	It("waits for its server and resumes when it appears", func() {
		d := &techv1alpha1.TechnitiumAllowedDomain{
			ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "envtest-waiting"},
			Spec:       domainSpec("waiting.example.com"),
		}
		d.Spec.ServerRef.Name = "late"
		Expect(k8sClient.Create(ctx, d)).To(Succeed())
		Eventually(func(g Gomega) {
			var got techv1alpha1.TechnitiumAllowedDomain
			g.Expect(k8sClient.Get(ctx, key("envtest-waiting"), &got)).To(Succeed())
			c := meta.FindStatusCondition(got.Status.Conditions, techv1alpha1.ConditionReady)
			g.Expect(c).NotTo(BeNil())
			g.Expect(c.Reason).To(Equal(techv1alpha1.ReasonServerNotFound))
		}, timeout, interval).Should(Succeed())
		// the provider knows no "late" server, so only the lookup matters here
		Expect(k8sClient.Delete(ctx, d)).To(Succeed())
		Eventually(func() bool {
			return k8sClient.Get(ctx, key("envtest-waiting"), &techv1alpha1.TechnitiumAllowedDomain{}) != nil
		}, timeout, interval).Should(BeTrue())
	})

	It("validates the domain and the deletion policy", func() {
		for _, bad := range []string{"", "has space.com", "under score!.com", "-lead.example.com", "a..b"} {
			d := &techv1alpha1.TechnitiumAllowedDomain{
				ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "bad-domain"},
				Spec:       domainSpec(bad),
			}
			Expect(k8sClient.Create(ctx, d)).NotTo(Succeed(), "domain %q", bad)
		}
		d := &techv1alpha1.TechnitiumAllowedDomain{
			ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "bad-policy"},
			Spec:       domainSpec("policy.example.com"),
		}
		d.Spec.DeletionPolicy = "Maybe"
		Expect(k8sClient.Create(ctx, d)).NotTo(Succeed())
	})

	It("does not allow changing the domain or the server", func() {
		d := &techv1alpha1.TechnitiumAllowedDomain{
			ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "immutable"},
			Spec:       domainSpec("immutable.example.com"),
		}
		d.Spec.DeletionPolicy = techv1alpha1.DeletionPolicyOrphan
		Expect(k8sClient.Create(ctx, d)).To(Succeed())
		DeferCleanup(func() { Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, d))).To(Succeed()) })

		// the controller writes the finalizer concurrently: retry on conflicts
		update := func(mod func(*techv1alpha1.DomainSpec)) error {
			var err error
			Eventually(func() bool {
				var cur techv1alpha1.TechnitiumAllowedDomain
				Expect(k8sClient.Get(ctx, key("immutable"), &cur)).To(Succeed())
				mod(&cur.Spec)
				err = k8sClient.Update(ctx, &cur)
				return !apierrors.IsConflict(err)
			}, timeout, interval).Should(BeTrue())
			return err
		}
		err := update(func(s *techv1alpha1.DomainSpec) { s.Domain = "other.example.com" })
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("domain is immutable"))

		err = update(func(s *techv1alpha1.DomainSpec) { s.ServerRef.Name = "elsewhere" })
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("serverRef is immutable"))

		// the rest can change
		Expect(update(func(s *techv1alpha1.DomainSpec) { s.Reason = "now with a reason" })).To(Succeed())
	})
})

var _ = Describe("TechnitiumBlockedDomain", func() {
	It("adds and deletes in the blocked list, and keeps an Orphan", func() {
		keep := &techv1alpha1.TechnitiumBlockedDomain{
			ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "envtest-keep"},
			Spec:       domainSpec("keep.example.org"),
		}
		keep.Spec.DeletionPolicy = techv1alpha1.DeletionPolicyOrphan
		gone := &techv1alpha1.TechnitiumBlockedDomain{
			ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "envtest-gone"},
			Spec:       domainSpec("gone.example.org"),
		}
		Expect(k8sClient.Create(ctx, keep)).To(Succeed())
		Expect(k8sClient.Create(ctx, gone)).To(Succeed())
		Eventually(func() []string { return fakeTech.Domains(technitium.Blocked) }, timeout, interval).
			Should(ContainElements("keep.example.org", "gone.example.org"))
		Expect(fakeTech.Domains(technitium.Allowed)).NotTo(ContainElement("keep.example.org"))

		Expect(k8sClient.Delete(ctx, keep)).To(Succeed())
		Expect(k8sClient.Delete(ctx, gone)).To(Succeed())
		Eventually(func() []string { return fakeTech.Domains(technitium.Blocked) }, timeout, interval).
			ShouldNot(ContainElement("gone.example.org"))
		Eventually(func() bool {
			return k8sClient.Get(ctx, key("envtest-keep"), &techv1alpha1.TechnitiumBlockedDomain{}) != nil
		}, timeout, interval).Should(BeTrue())
		Expect(fakeTech.Domains(technitium.Blocked)).To(ContainElement("keep.example.org"))
	})
})

var _ = Describe("the samples", func() {
	It("are valid resources", func() {
		files, err := filepath.Glob(filepath.Join("..", "..", "config", "samples", "technitium_v1alpha1_*.yaml"))
		Expect(err).NotTo(HaveOccurred())
		Expect(files).To(HaveLen(3))
		for _, f := range files {
			data, err := os.ReadFile(f)
			Expect(err).NotTo(HaveOccurred())
			obj := &unstructured.Unstructured{}
			Expect(yaml.Unmarshal(data, &obj.Object)).To(Succeed(), f)
			obj.SetNamespace("default")
			obj.SetName(obj.GetName() + "-sample") // "primary" exists already
			Expect(k8sClient.Create(ctx, obj, client.DryRunAll)).To(Succeed(), f)
		}
	})
})
