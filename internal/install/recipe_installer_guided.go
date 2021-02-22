package install

import (
	"fmt"

	log "github.com/sirupsen/logrus"

	"github.com/newrelic/newrelic-cli/internal/install/execution"
	"github.com/newrelic/newrelic-cli/internal/install/types"
	"github.com/newrelic/newrelic-cli/internal/utils"
)

const (
	infraAgentRecipeName = "infrastructure-agent-installer"
	loggingRecipeName    = "logs-integration"
)

func (i *RecipeInstaller) guidedInstall(m *types.DiscoveryManifest) error {
	var recipesForInstallation []types.Recipe
	var integrationsForInstallation []types.Recipe
	var recommendedIntegrations []types.Recipe

	// Fetch the infra agent recipe and mark it as available.
	infraAgentRecipe, err := i.fetchRecipeAndReportAvailable(m, infraAgentRecipeName)
	if err != nil {
		return err
	}
	recipesForInstallation = append(recipesForInstallation, *infraAgentRecipe)

	// Fetch the logging recipe and mark it as available.
	loggingRecipe, err := i.fetchRecipeAndReportAvailable(m, loggingRecipeName)
	if err != nil {
		return err
	}

	// Mark the logging recipe as skipped if necessary.
	if i.SkipLoggingInstall {
		i.status.RecipeSkipped(execution.RecipeStatusEvent{Recipe: *loggingRecipe})
	} else {
		recipesForInstallation = append(recipesForInstallation, *loggingRecipe)
	}

	// If necessary, fetch additional integration recommendations from the recipe service.
	if !i.SkipDiscovery {
		recommendedIntegrations, err = i.fetchRecommendations(m)
		if err != nil {
			log.Debugf("error fetching additional integrations: %s", err)
			return err
		}

		if len(recommendedIntegrations) == 0 {
			log.Debug("no additional integrations found")
		}

		// Filter integrations, based on recipe metadata, command flags and prompts.
		integrationsForInstallation, err = i.filterIntegrations(recommendedIntegrations)
		if err != nil {
			return err
		}
		recipesForInstallation = append(recipesForInstallation, integrationsForInstallation...)
		i.status.RecipesAvailable(integrationsForInstallation)
	}

	// Show the user what will be installed.
	i.status.RecipesSelected(recipesForInstallation)

	// Install the infra agent.
	log.Debugf("Installing infrastructure agent")
	entityGUID, err := i.executeAndValidateWithProgress(m, infraAgentRecipe)
	if err != nil {
		log.Error(i.failMessage(infraAgentRecipeName))
		return err
	}
	log.Debugf("Done installing infrastructure agent.")

	// Now that we have a host entity GUID, report recommended integrations
	// with application targets for that host.
	for _, r := range recommendedIntegrations {
		if r.HasApplicationTargetType() {
			i.status.RecipeRecommended(execution.RecipeStatusEvent{
				Recipe:     r,
				EntityGUID: entityGUID,
			})
		}
	}

	// Install logging if necessary.
	if i.ShouldInstallLogging() {
		log.Debugf("Installing logging")
		if err = i.installLogging(m, loggingRecipe, recipesForInstallation); err != nil {
			log.Error(i.failMessage(loggingRecipeName))
			return err
		}
		log.Debugf("Done installing logging.")
	}

	// Install integrations if necessary, continuing on failure with warnings.
	if i.ShouldInstallIntegrations() {
		log.Debugf("Installing integrations")
		if err = i.installRecipes(m, integrationsForInstallation); err != nil {
			return err
		}
		log.Debugf("Done installing integrations.")
	}

	return nil
}

func (i *RecipeInstaller) installLogging(m *types.DiscoveryManifest, r *types.Recipe, recipes []types.Recipe) error {
	log.WithFields(log.Fields{
		"recipe_count": len(recipes),
	}).Debug("filtering log matches")
	logMatches, err := i.fileFilterer.Filter(utils.SignalCtx, recipes)
	if err != nil {
		return err
	}

	log.WithFields(log.Fields{
		"possible_matches": len(logMatches),
	}).Debug("filtered log matches")

	var acceptedLogMatches []types.LogMatch
	var ok bool
	for _, match := range logMatches {
		ok, err = i.userAcceptsLogFile(match)
		if err != nil {
			return err
		}

		if ok {
			acceptedLogMatches = append(acceptedLogMatches, match)
		}
	}

	log.WithFields(log.Fields{
		"matches": acceptedLogMatches,
	}).Debug("matches accepted")

	// The struct to approximate the logging configuration file of the Infra Agent.
	type loggingConfig struct {
		Logs []types.LogMatch `yaml:"logs"`
	}

	r.AddVar("DISCOVERED_LOG_FILES", loggingConfig{Logs: acceptedLogMatches})

	_, err = i.executeAndValidateWithProgress(m, r)
	return err
}

func (i *RecipeInstaller) fetchRecommendations(m *types.DiscoveryManifest) ([]types.Recipe, error) {
	log.Debug("fetching recommended recipes")

	recipes, err := i.recipeFetcher.FetchRecommendations(utils.SignalCtx, m)
	if err != nil {
		return nil, fmt.Errorf("error retrieving recipe recommendations: %s", err)
	}

	filteredRecommendations := i.filterRecommendations(recipes)

	if log.IsLevelEnabled(log.DebugLevel) {
		names := []string{}
		for _, r := range recipes {
			names = append(names, r.Name)
		}

		log.WithFields(log.Fields{
			"names":        names,
			"recipe_count": len(recipes),
		}).Debug("recommended integrations")
	}

	return filteredRecommendations, nil
}

// Filter out infra and logging recipes from recommendations, since they are
// handled explicitly elsewhere.  This avoids duplicate installation.
func (i *RecipeInstaller) filterRecommendations(recipes []types.Recipe) []types.Recipe {
	filteredRecommendations := []types.Recipe{}
	for _, r := range recipes {
		if r.Name == infraAgentRecipeName || r.Name == loggingRecipeName {
			log.WithFields(log.Fields{
				"name": r.Name,
			}).Debug("skipping redundant recipe")

			continue
		}

		filteredRecommendations = append(filteredRecommendations, r)
	}

	return filteredRecommendations
}

func (i *RecipeInstaller) userAcceptsLogFile(match types.LogMatch) (bool, error) {
	msg := fmt.Sprintf("Files have been found at the following pattern: %s Do you want to watch them?", match.File)
	return i.userAccepts(msg)
}

func (i *RecipeInstaller) recipeInRecipes(recipe types.Recipe, recipes []types.Recipe) bool {
	for _, r := range recipes {
		if recipe.Name == r.Name {
			return true
		}
	}

	return false
}

func (i *RecipeInstaller) filterIntegrations(recommendedIntegrations []types.Recipe) ([]types.Recipe, error) {
	installCandidates := []types.Recipe{}
	for _, r := range recommendedIntegrations {
		if r.HasApplicationTargetType() {
			// do nothing
		} else if i.SkipIntegrations {
			i.status.RecipeSkipped(execution.RecipeStatusEvent{Recipe: r})
		} else {
			installCandidates = append(installCandidates, r)
		}
	}

	installCandidateNames := []string{}
	for _, r := range installCandidates {
		installCandidateNames = append(installCandidateNames, r.DisplayName)
	}

	var selectedIntegrationNames []string
	if i.AssumeYes {
		// When -y is supplied, select all the recipes that were in the report for install.
		selectedIntegrationNames = installCandidateNames
	} else {
		fmt.Printf("The guided installation will begin by installing the New Relic Infrastructure agent, which is required for additional instrumentation.\n\n")

		var promptErr error
		selectedIntegrationNames, promptErr = i.prompter.MultiSelect("Please choose from the additional recommended instrumentation to be installed:", installCandidateNames)
		if promptErr != nil {
			return nil, promptErr
		}

		fmt.Println()
	}

	var integrationsForInstall []types.Recipe
	for _, selectedIntegrationName := range selectedIntegrationNames {
		for _, r := range recommendedIntegrations {
			if r.DisplayName == selectedIntegrationName {
				integrationsForInstall = append(integrationsForInstall, r)
			}
		}
	}

	log.Debug("skipping recipes that were not selected")
	for _, r := range recommendedIntegrations {
		if !i.recipeInRecipes(r, integrationsForInstall) {
			i.status.RecipeSkipped(execution.RecipeStatusEvent{Recipe: r})
		}
	}

	return integrationsForInstall, nil
}
