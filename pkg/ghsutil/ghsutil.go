package ghsutil

import (
	"context"
	"time"

	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	githubappjwt2tokenv1 "github.com/technicaldomain/github-app-jwt2token-controller/pkg/apis/githubappjwt2token/v1"
	clientset "github.com/technicaldomain/github-app-jwt2token-controller/pkg/generated/clientset/versioned"
)

func CreateOrUpdateGHS(ctx context.Context, clientset clientset.Interface, tokenHash, token, namespace string, expiresAt time.Time) error {
	ghs, err := clientset.GithubappV1().GHSs(namespace).Get(ctx, tokenHash, metav1.GetOptions{})
	if errors.IsNotFound(err) {
		ghs = &githubappjwt2tokenv1.GHS{
			ObjectMeta: metav1.ObjectMeta{
				Name: tokenHash,
			},
			Spec: githubappjwt2tokenv1.GHSSpec{
				Token: token,
			},
		}
		ghs, err = clientset.GithubappV1().GHSs(namespace).Create(ctx, ghs, metav1.CreateOptions{})
		if err != nil {
			return err
		}
		ghs.Status.ExpiresAt = metav1.NewTime(expiresAt)
		_, err = clientset.GithubappV1().GHSs(namespace).UpdateStatus(ctx, ghs, metav1.UpdateOptions{})
		if err != nil {
			return err
		}
	} else if err == nil {
		ghsCopy := ghs.DeepCopy()
		ghsCopy.Spec.Token = token
		ghsCopy.Status.ExpiresAt = metav1.NewTime(expiresAt)
		_, err = clientset.GithubappV1().GHSs(namespace).Update(ctx, ghsCopy, metav1.UpdateOptions{})
		if err != nil {
			return err
		}
	} else {
		return err
	}
	return nil
}

func GetGHS(ctx context.Context, clientset clientset.Interface, tokenHash, namespace string) (*githubappjwt2tokenv1.GHS, error) {
	return clientset.GithubappV1().GHSs(namespace).Get(ctx, tokenHash, metav1.GetOptions{})
}

func DeleteGHS(ctx context.Context, clientset clientset.Interface, tokenHash, namespace string) error {
	return clientset.GithubappV1().GHSs(namespace).Delete(ctx, tokenHash, metav1.DeleteOptions{})
}

func DeleteExpiredGHS(ctx context.Context, clientset clientset.Interface, namespace string) error {
	ghsList, err := clientset.GithubappV1().GHSs(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return err
	}
	for _, ghs := range ghsList.Items {
		if ghs.Status.ExpiresAt.Time.Before(time.Now().Add(-15 * time.Minute)) {
			err = clientset.GithubappV1().GHSs(namespace).Delete(ctx, ghs.Name, metav1.DeleteOptions{})
			if err != nil {
				return err
			}
		}
	}
	return nil
}
