package scene

import (
	"context"
	"fmt"
	"strings"

	"github.com/stashapp/stash/pkg/file"
	"github.com/stashapp/stash/pkg/gallery"
	"github.com/stashapp/stash/pkg/models"
	"github.com/stashapp/stash/pkg/models/jsonschema"
	"github.com/stashapp/stash/pkg/movie"
	"github.com/stashapp/stash/pkg/performer"
	"github.com/stashapp/stash/pkg/sliceutil/stringslice"
	"github.com/stashapp/stash/pkg/studio"
	"github.com/stashapp/stash/pkg/tag"
	"github.com/stashapp/stash/pkg/utils"
)

type FullCreatorUpdater interface {
	CreatorUpdater
	Update(ctx context.Context, updatedScene *models.Scene) error
	Updater
}

type Importer struct {
	ReaderWriter        FullCreatorUpdater
	FileFinder          file.Getter
	StudioWriter        studio.NameFinderCreator
	GalleryFinder       gallery.Finder
	PerformerWriter     performer.NameFinderCreator
	MovieWriter         movie.NameFinderCreator
	TagWriter           tag.NameFinderCreator
	Input               jsonschema.Scene
	MissingRefBehaviour models.ImportMissingRefEnum
	FileNamingAlgorithm models.HashAlgorithm

	ID             int
	scene          models.Scene
	coverImageData []byte
}

func (i *Importer) PreImport(ctx context.Context) error {
	i.scene = i.sceneJSONToScene(i.Input)

	if err := i.populateFiles(ctx); err != nil {
		return err
	}

	if err := i.populateStudio(ctx); err != nil {
		return err
	}

	if err := i.populateGalleries(ctx); err != nil {
		return err
	}

	if err := i.populatePerformers(ctx); err != nil {
		return err
	}

	if err := i.populateTags(ctx); err != nil {
		return err
	}

	if err := i.populateMovies(ctx); err != nil {
		return err
	}

	var err error
	if len(i.Input.Cover) > 0 {
		i.coverImageData, err = utils.ProcessBase64Image(i.Input.Cover)
		if err != nil {
			return fmt.Errorf("invalid cover image: %v", err)
		}
	}

	return nil
}

func (i *Importer) sceneJSONToScene(sceneJSON jsonschema.Scene) models.Scene {
	newScene := models.Scene{
		// Path:    i.Path,
		Title:        sceneJSON.Title,
		Code:         sceneJSON.Code,
		Details:      sceneJSON.Details,
		Director:     sceneJSON.Director,
		URL:          sceneJSON.URL,
		PerformerIDs: models.NewRelatedIDs([]int{}),
		TagIDs:       models.NewRelatedIDs([]int{}),
		GalleryIDs:   models.NewRelatedIDs([]int{}),
		Movies:       models.NewRelatedMovies([]models.MoviesScenes{}),
		StashIDs:     models.NewRelatedStashIDs(sceneJSON.StashIDs),
	}

	if sceneJSON.Date != "" {
		d := models.NewDate(sceneJSON.Date)
		newScene.Date = &d
	}
	if sceneJSON.Rating != 0 {
		newScene.Rating = &sceneJSON.Rating
	}

	newScene.Organized = sceneJSON.Organized
	newScene.OCounter = sceneJSON.OCounter
	newScene.CreatedAt = sceneJSON.CreatedAt.GetTime()
	newScene.UpdatedAt = sceneJSON.UpdatedAt.GetTime()

	return newScene
}

func (i *Importer) populateFiles(ctx context.Context) error {
	files := make([]*file.VideoFile, 0)

	for _, ref := range i.Input.Files {
		path := ref
		f, err := i.FileFinder.FindByPath(ctx, path)
		if err != nil {
			return fmt.Errorf("error finding file: %w", err)
		}

		if f == nil {
			return fmt.Errorf("scene file '%s' not found", path)
		} else {
			files = append(files, f.(*file.VideoFile))
		}
	}

	i.scene.Files = models.NewRelatedVideoFiles(files)

	return nil
}

func (i *Importer) populateStudio(ctx context.Context) error {
	if i.Input.Studio != "" {
		studio, err := i.StudioWriter.FindByName(ctx, i.Input.Studio, false)
		if err != nil {
			return fmt.Errorf("error finding studio by name: %v", err)
		}

		if studio == nil {
			if i.MissingRefBehaviour == models.ImportMissingRefEnumFail {
				return fmt.Errorf("scene studio '%s' not found", i.Input.Studio)
			}

			if i.MissingRefBehaviour == models.ImportMissingRefEnumIgnore {
				return nil
			}

			if i.MissingRefBehaviour == models.ImportMissingRefEnumCreate {
				studioID, err := i.createStudio(ctx, i.Input.Studio)
				if err != nil {
					return err
				}
				i.scene.StudioID = &studioID
			}
		} else {
			i.scene.StudioID = &studio.ID
		}
	}

	return nil
}

func (i *Importer) createStudio(ctx context.Context, name string) (int, error) {
	newStudio := *models.NewStudio(name)

	created, err := i.StudioWriter.Create(ctx, newStudio)
	if err != nil {
		return 0, err
	}

	return created.ID, nil
}

func (i *Importer) locateGallery(ctx context.Context, ref jsonschema.GalleryRef) (*models.Gallery, error) {
	var galleries []*models.Gallery
	var err error
	switch {
	case ref.FolderPath != "":
		galleries, err = i.GalleryFinder.FindByPath(ctx, ref.FolderPath)
	case len(ref.ZipFiles) > 0:
		for _, p := range ref.ZipFiles {
			galleries, err = i.GalleryFinder.FindByPath(ctx, p)
			if err != nil {
				break
			}

			if len(galleries) > 0 {
				break
			}
		}
	case ref.Title != "":
		galleries, err = i.GalleryFinder.FindUserGalleryByTitle(ctx, ref.Title)
	}

	var ret *models.Gallery
	if len(galleries) > 0 {
		ret = galleries[0]
	}

	return ret, err
}

func (i *Importer) populateGalleries(ctx context.Context) error {
	for _, ref := range i.Input.Galleries {
		gallery, err := i.locateGallery(ctx, ref)
		if err != nil {
			return err
		}

		if gallery == nil {
			if i.MissingRefBehaviour == models.ImportMissingRefEnumFail {
				return fmt.Errorf("scene gallery '%s' not found", ref.String())
			}

			// we don't create galleries - just ignore
		} else {
			i.scene.GalleryIDs.Add(gallery.ID)
		}
	}

	return nil
}

func (i *Importer) populatePerformers(ctx context.Context) error {
	if len(i.Input.Performers) > 0 {
		names := i.Input.Performers
		performers, err := i.PerformerWriter.FindByNames(ctx, names, false)
		if err != nil {
			return err
		}

		var pluckedNames []string
		for _, performer := range performers {
			if performer.Name == "" {
				continue
			}
			pluckedNames = append(pluckedNames, performer.Name)
		}

		missingPerformers := stringslice.StrFilter(names, func(name string) bool {
			return !stringslice.StrInclude(pluckedNames, name)
		})

		if len(missingPerformers) > 0 {
			if i.MissingRefBehaviour == models.ImportMissingRefEnumFail {
				return fmt.Errorf("scene performers [%s] not found", strings.Join(missingPerformers, ", "))
			}

			if i.MissingRefBehaviour == models.ImportMissingRefEnumCreate {
				createdPerformers, err := i.createPerformers(ctx, missingPerformers)
				if err != nil {
					return fmt.Errorf("error creating scene performers: %v", err)
				}

				performers = append(performers, createdPerformers...)
			}

			// ignore if MissingRefBehaviour set to Ignore
		}

		for _, p := range performers {
			i.scene.PerformerIDs.Add(p.ID)
		}
	}

	return nil
}

func (i *Importer) createPerformers(ctx context.Context, names []string) ([]*models.Performer, error) {
	var ret []*models.Performer
	for _, name := range names {
		newPerformer := *models.NewPerformer(name)

		err := i.PerformerWriter.Create(ctx, &newPerformer)
		if err != nil {
			return nil, err
		}

		ret = append(ret, &newPerformer)
	}

	return ret, nil
}

func (i *Importer) populateMovies(ctx context.Context) error {
	if len(i.Input.Movies) > 0 {
		for _, inputMovie := range i.Input.Movies {
			movie, err := i.MovieWriter.FindByName(ctx, inputMovie.MovieName, false)
			if err != nil {
				return fmt.Errorf("error finding scene movie: %v", err)
			}

			if movie == nil {
				if i.MissingRefBehaviour == models.ImportMissingRefEnumFail {
					return fmt.Errorf("scene movie [%s] not found", inputMovie.MovieName)
				}

				if i.MissingRefBehaviour == models.ImportMissingRefEnumCreate {
					movie, err = i.createMovie(ctx, inputMovie.MovieName)
					if err != nil {
						return fmt.Errorf("error creating scene movie: %v", err)
					}
				}

				// ignore if MissingRefBehaviour set to Ignore
				if i.MissingRefBehaviour == models.ImportMissingRefEnumIgnore {
					continue
				}
			}

			toAdd := models.MoviesScenes{
				MovieID: movie.ID,
			}

			if inputMovie.SceneIndex != 0 {
				index := inputMovie.SceneIndex
				toAdd.SceneIndex = &index
			}

			i.scene.Movies.Add(toAdd)
		}
	}

	return nil
}

func (i *Importer) createMovie(ctx context.Context, name string) (*models.Movie, error) {
	newMovie := *models.NewMovie(name)

	created, err := i.MovieWriter.Create(ctx, newMovie)
	if err != nil {
		return nil, err
	}

	return created, nil
}

func (i *Importer) populateTags(ctx context.Context) error {
	if len(i.Input.Tags) > 0 {

		tags, err := importTags(ctx, i.TagWriter, i.Input.Tags, i.MissingRefBehaviour)
		if err != nil {
			return err
		}

		for _, p := range tags {
			i.scene.TagIDs.Add(p.ID)
		}
	}

	return nil
}

func (i *Importer) PostImport(ctx context.Context, id int) error {
	if len(i.coverImageData) > 0 {
		if err := i.ReaderWriter.UpdateCover(ctx, id, i.coverImageData); err != nil {
			return fmt.Errorf("error setting scene images: %v", err)
		}
	}

	return nil
}

func (i *Importer) Name() string {
	if i.Input.Title != "" {
		return i.Input.Title
	}

	if len(i.Input.Files) > 0 {
		return i.Input.Files[0]
	}

	return ""
}

func (i *Importer) FindExistingID(ctx context.Context) (*int, error) {
	var existing []*models.Scene
	var err error

	for _, f := range i.scene.Files.List() {
		existing, err = i.ReaderWriter.FindByFileID(ctx, f.ID)
		if err != nil {
			return nil, err
		}

		if len(existing) > 0 {
			id := existing[0].ID
			return &id, nil
		}
	}

	return nil, nil
}

func (i *Importer) Create(ctx context.Context) (*int, error) {
	var fileIDs []file.ID
	for _, f := range i.scene.Files.List() {
		fileIDs = append(fileIDs, f.Base().ID)
	}
	if err := i.ReaderWriter.Create(ctx, &i.scene, fileIDs); err != nil {
		return nil, fmt.Errorf("error creating scene: %v", err)
	}

	id := i.scene.ID
	i.ID = id
	return &id, nil
}

func (i *Importer) Update(ctx context.Context, id int) error {
	scene := i.scene
	scene.ID = id
	i.ID = id
	if err := i.ReaderWriter.Update(ctx, &scene); err != nil {
		return fmt.Errorf("error updating existing scene: %v", err)
	}

	return nil
}

func importTags(ctx context.Context, tagWriter tag.NameFinderCreator, names []string, missingRefBehaviour models.ImportMissingRefEnum) ([]*models.Tag, error) {
	tags, err := tagWriter.FindByNames(ctx, names, false)
	if err != nil {
		return nil, err
	}

	var pluckedNames []string
	for _, tag := range tags {
		pluckedNames = append(pluckedNames, tag.Name)
	}

	missingTags := stringslice.StrFilter(names, func(name string) bool {
		return !stringslice.StrInclude(pluckedNames, name)
	})

	if len(missingTags) > 0 {
		if missingRefBehaviour == models.ImportMissingRefEnumFail {
			return nil, fmt.Errorf("tags [%s] not found", strings.Join(missingTags, ", "))
		}

		if missingRefBehaviour == models.ImportMissingRefEnumCreate {
			createdTags, err := createTags(ctx, tagWriter, missingTags)
			if err != nil {
				return nil, fmt.Errorf("error creating tags: %v", err)
			}

			tags = append(tags, createdTags...)
		}

		// ignore if MissingRefBehaviour set to Ignore
	}

	return tags, nil
}

func createTags(ctx context.Context, tagWriter tag.NameFinderCreator, names []string) ([]*models.Tag, error) {
	var ret []*models.Tag
	for _, name := range names {
		newTag := *models.NewTag(name)

		created, err := tagWriter.Create(ctx, newTag)
		if err != nil {
			return nil, err
		}

		ret = append(ret, created)
	}

	return ret, nil
}
