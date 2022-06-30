package environment

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	"github.com/google/go-github/v41/github"
	"github.com/porter-dev/porter/api/server/handlers"
	"github.com/porter-dev/porter/api/server/shared"
	"github.com/porter-dev/porter/api/server/shared/apierrors"
	"github.com/porter-dev/porter/api/server/shared/commonutils"
	"github.com/porter-dev/porter/api/server/shared/config"
	"github.com/porter-dev/porter/api/types"
	"github.com/porter-dev/porter/internal/models"
	"github.com/porter-dev/porter/internal/models/integrations"
	"github.com/porter-dev/porter/internal/repository"
)

type FinalizeDeploymentHandler struct {
	handlers.PorterHandlerReadWriter
}

func NewFinalizeDeploymentHandler(
	config *config.Config,
	decoderValidator shared.RequestDecoderValidator,
	writer shared.ResultWriter,
) *FinalizeDeploymentHandler {
	return &FinalizeDeploymentHandler{
		PorterHandlerReadWriter: handlers.NewDefaultPorterHandler(config, decoderValidator, writer),
	}
}

func (c *FinalizeDeploymentHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	ga, _ := r.Context().Value(types.GitInstallationScope).(*integrations.GithubAppInstallation)
	project, _ := r.Context().Value(types.ProjectScope).(*models.Project)
	cluster, _ := r.Context().Value(types.ClusterScope).(*models.Cluster)

	owner, name, ok := commonutils.GetOwnerAndNameParams(c, w, r)

	if !ok {
		return
	}

	request := &types.FinalizeDeploymentRequest{}

	if ok := c.DecodeAndValidate(w, r, request); !ok {
		return
	}

	// read the environment to get the environment id
	env, err := c.Repo().Environment().ReadEnvironment(project.ID, cluster.ID, uint(ga.InstallationID), owner, name)

	if err != nil {
		c.HandleAPIError(w, r, apierrors.NewErrInternal(err))
		return
	}

	// read the deployment
	depl, err := c.Repo().Environment().ReadDeployment(env.ID, request.Namespace)

	if err != nil {
		c.HandleAPIError(w, r, apierrors.NewErrInternal(err))
		return
	}

	depl.Subdomain = request.Subdomain
	depl.Status = types.DeploymentStatusCreated

	// update the deployment
	depl, err = c.Repo().Environment().UpdateDeployment(depl)

	if err != nil {
		c.HandleAPIError(w, r, apierrors.NewErrInternal(err))
		return
	}

	client, err := getGithubClientFromEnvironment(c.Config(), env)

	if err != nil {
		c.HandleAPIError(w, r, apierrors.NewErrInternal(err))
		return
	}

	// Create new deployment status to indicate deployment is ready

	state := "success"
	env_url := depl.Subdomain

	deploymentStatusRequest := github.DeploymentStatusRequest{
		State:          &state,
		EnvironmentURL: &env_url,
	}

	_, _, err = client.Repositories.CreateDeploymentStatus(
		context.Background(),
		env.GitRepoOwner,
		env.GitRepoName,
		depl.GHDeploymentID,
		&deploymentStatusRequest,
	)

	if err != nil {
		c.HandleAPIError(w, r, apierrors.NewErrInternal(err))
		return
	}

	workflowRun, err := commonutils.GetLatestWorkflowRun(client, depl.RepoOwner, depl.RepoName,
		fmt.Sprintf("porter_%s_env.yml", env.Name), depl.PRBranchFrom)

	if err != nil {
		c.HandleAPIError(w, r, apierrors.NewErrInternal(err))
		return
	}

	if depl.Subdomain == "" {
		depl.Subdomain = "*Ingress is disabled for this deployment*"
	}

	// write comment in PR
	commentBody := fmt.Sprintf(
		"## Porter Preview Environments\n"+
			"✅ All changes deployed successfully\n"+
			"||Deployment Information|\n"+
			"|-|-|\n"+
			"| Latest SHA | [`%s`](https://github.com/%s/%s/commit/%s) |\n"+
			"| Live URL | %s |\n"+
			"| Build Logs | %s |\n"+
			"| Porter Deployments URL | %s/preview-environments/details/%s?environment_id=%d |",
		depl.CommitSHA, depl.RepoOwner, depl.RepoName, depl.CommitSHA, depl.Subdomain, workflowRun.GetHTMLURL(),
		c.Config().ServerConf.ServerURL, depl.Namespace, depl.EnvironmentID,
	)

	if len(request.SuccessfulResources) > 0 {
		commentBody += "\n#### Successfully deployed resources\n"

		for _, res := range request.SuccessfulResources {
			if res.ReleaseType == "job" {
				commentBody += fmt.Sprintf("- [`%s`](%s/jobs/%s/%s/%s?project_id=%d)\n",
					res.ReleaseName, c.Config().ServerConf.ServerURL, cluster.Name, depl.Namespace,
					res.ReleaseName, project.ID)
			} else {
				commentBody += fmt.Sprintf("- [`%s`](%s/applications/%s/%s/%s?project_id=%d)\n",
					res.ReleaseName, c.Config().ServerConf.ServerURL, cluster.Name, depl.Namespace,
					res.ReleaseName, project.ID)
			}
		}
	}

	err = createOrUpdateComment(client, c.Repo(), env.NewCommentsDisabled, depl, github.String(commentBody))

	if err != nil {
		c.HandleAPIError(w, r, apierrors.NewErrInternal(err))
		return
	}

	c.WriteResult(w, r, depl.ToDeploymentType())
}

func createOrUpdateComment(
	client *github.Client,
	repo repository.Repository,
	newCommentsDisabled bool,
	depl *models.Deployment,
	commentBody *string,
) error {
	// when updating a PR comment, we have to handle several cases:
	//   1. when a Porter environment has deployment status repeat-comments enabled
	//      - nothing special here, simply create a new comment in the PR
	//   2. when a Porter environment has deployment status repeat-comments disabled
	//      - when a Porter deployment has Github comment ID saved in the DB
	//        - try to update the comment using the Github comment ID
	//        - if the above fails, try creating a new comment and save the new comment ID in the DB
	//      - when a Porter deployment does not have a Github comment ID saved in the DB
	//        - create a new comment and save the Github comment ID in the DB

	if newCommentsDisabled {
		if depl.GHPRCommentID == 0 {
			// create a new comment
			err := createGithubComment(client, repo, depl, commentBody)

			if err != nil {
				return err
			}
		} else {
			err := updateGithubComment(
				client, depl.RepoOwner, depl.RepoName, depl.GHPRCommentID, commentBody,
			)

			if err != nil {
				if strings.Contains(err.Error(), "404") {
					// perhaps a deleted comment?
					// create a new comment
					err := createGithubComment(client, repo, depl, commentBody)

					if err != nil {
						return fmt.Errorf("invalid github comment ID for deployment with ID: %d. Error creating "+
							"new comment: %w", depl.ID, err)
					}
				}

				return err
			}
		}
	} else {
		err := createGithubComment(client, repo, depl, commentBody)

		if err != nil {
			return err
		}
	}

	return nil
}

func createGithubComment(
	client *github.Client,
	repo repository.Repository,
	depl *models.Deployment,
	body *string,
) error {
	ghResp, _, err := client.Issues.CreateComment(
		context.Background(),
		depl.RepoOwner,
		depl.RepoName,
		int(depl.PullRequestID),
		&github.IssueComment{
			Body: body,
		},
	)

	if err != nil {
		return fmt.Errorf("error creating new github comment for owner: %s repo %s prNumber: %d. Error: %w",
			depl.RepoOwner, depl.RepoName, depl.PullRequestID, err)
	}

	depl.GHPRCommentID = ghResp.GetID()

	_, err = repo.Environment().UpdateDeployment(depl)

	if err != nil {
		return fmt.Errorf("error updating deployment with ID: %d. Error: %w", depl.ID, err)
	}

	return nil
}

func updateGithubComment(
	client *github.Client,
	owner, repo string,
	commentID int64,
	body *string,
) error {
	_, _, err := client.Issues.EditComment(
		context.Background(),
		owner,
		repo,
		commentID,
		&github.IssueComment{
			Body: body,
		},
	)

	return err
}
