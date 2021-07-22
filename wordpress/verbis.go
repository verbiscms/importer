// Copyright 2020 The Verbis Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package wordpress

import (
	"fmt"
	"github.com/ainsleyclark/verbis/api/common/encryption"
	"github.com/ainsleyclark/verbis/api/domain"
	"github.com/ainsleyclark/verbis/api/errors"
	"github.com/ainsleyclark/verbis/api/importer"
	"github.com/ainsleyclark/verbis/api/store"
	"github.com/gookit/color"
	"github.com/kyokomi/emoji"
	"mime/multipart"
	"runtime"
	"strings"
	"sync"
)

const maxCPUNum = 4

// TODO: This needs to be dynamic.
var (
	resource   = "news"
	layout     = "main"
	template   = "news-single"
	fieldUUID  = "2dedc760-5016-11eb-ae93-0242ac130002"
	userRoleID = 2
	trackChan  = make(chan int, runtime.NumCPU()*maxCPUNum)
	wg         = sync.WaitGroup{}
)

type Convert struct {
	XML       WpXML
	failed    Failures
	store     *store.Repository
	authors   domain.Users
	owner     domain.User
	sendEmail bool
}

type Result struct {
	Failed     Failures
	Posts      domain.PostData
	Authors    domain.UsersParts
	Categories domain.Categories
}

// New - Construct
func New(xmlPath string, s *store.Repository, sendEmail bool) (*Convert, error) {
	wp := NewWordpressXML()
	err := wp.ReadFile(xmlPath)
	if err != nil {
		return nil, err
	}

	return &Convert{
		XML:       wp,
		failed:    Failures{},
		store:     s,
		owner:     s.User.Owner(),
		sendEmail: sendEmail,
	}, nil
}

// Import
//
// The XML file into Wordpress by populating Authors
// and Posts.
func (c *Convert) Import() {
	authors := c.populateAuthors()
	posts, categories := c.populatePosts()

	r := Result{
		Failed:     c.failed,
		Posts:      posts,
		Authors:    authors,
		Categories: categories,
	}

	// TODO: To be returned here as a WebHook or placed in a Debug Table
	emoji.Println(":check_mark: Successful entries:")
	fmt.Printf("Posts: %d\n", len(r.Posts))
	fmt.Printf("Authors: %d\n", len(r.Authors))
	fmt.Printf("Categories: %d\n", len(r.Authors))
	fmt.Println()
	emoji.Println(":cross_mark: Failed entries")
	fmt.Printf("Posts: %d\n", len(r.Failed.Posts))
	fmt.Printf("Authors: %d\n", len(r.Failed.Authors))
}

// Failed import defines the errors that occurred when importing
// multiple entities into Verbis.
type Failures struct {
	Posts   []FailedPost
	Authors []FailedAuthor
}

// FailedMedia defines a failure of a post that occurred during migration.
type FailedPost struct {
	Post  Item
	Media []FailedMedia
	Error error
}

// FailedMedia defines a failure of an upload to the media library
type FailedMedia struct {
	URL   string
	Error error
}

// FailedAuthor defines a failure of an author that occurred during migration
type FailedAuthor struct {
	FirstName string
	LastName  string
	Email     string
	Error     error
}

var (
	posts      domain.PostData   // Successful posts that have been inserted
	categories domain.Categories // Successful categories that have been inserted
)

// populatePosts
//
// Loops over all of the Wordpress item and creates a Verbis post.
// Spawns a new process to insert into the database.
func (c *Convert) populatePosts() (domain.PostData, domain.Categories) {
	posts = domain.PostData{}
	categories = domain.Categories{}

	for _, item := range c.XML.Channel.Items {
		trackChan <- 1
		go c.addItem(item)
	}

	wg.Wait()

	return posts, categories
}

// addItem
//
// This function will append to the FailedPosts array if there
// was a problem parsing any of the content.
func (c *Convert) addItem(item Item) {
	wg.Add(1)
	defer func() {
		wg.Done()
		<-trackChan
	}()

	link, err := importer.ParseLink(item.Link)
	if err != nil {
		c.failPost(item, nil, err)
		return
	}

	uuid, err := importer.ParseUUID(fieldUUID)
	if err != nil {
		c.failPost(item, nil, err)
	}

	content, failed, err := c.parseContent(item.Content)
	if err != nil {
		c.failPost(item, failed, err)
	}

	p := domain.PostCreate{
		Post: domain.Post{
			Slug:         fmt.Sprintf("/%v/%v", resource, strings.ReplaceAll(link, "/", "")),
			Title:        item.Title,
			Status:       getStatus(item.Status),
			Resource:     resource,
			PageTemplate: template,
			PageLayout:   layout,
			PublishedAt:  &item.PubDatetime,
			CreatedAt:    item.PostDatetime,
			UpdatedAt:    item.PostDatetime,
			SeoMeta:      c.getSeoMeta(item.Title, item.Meta),
		},
		Author: c.findAuthor(item),
		Fields: domain.PostFields{
			{
				UUID:          uuid,
				Type:          "richtext",
				Name:          "content",
				OriginalValue: domain.FieldValue(content),
			},
		},
	}

	category, err := c.getCategory(item.Categories)
	if err != nil && errors.Code(err) != errors.NOTFOUND {
		c.failPost(item, nil, err)
		categories = append(categories, category)
	}

	if err == nil {
		cid := category.Id
		p.Category = &cid
	}

	post, err := c.store.Posts.Create(p)
	if err != nil {
		c.failPost(item, nil, err)
		return
	}

	posts = append(posts, post)
}

// parseContent
//
// Accepts a HTML document as a string and uses the ParseHTML function to
// loop over the images, upload them and modify the contents of the HTML
// file If a media item failed to be uploaded to the media library
// or a the file could not be downloaded (such as a 404) the
// media item will be appended to the FailedMedia array.
//
// Returns the modified HTML file, the FailedMedia array and an error
// if there was a problem parsing the HTML.
func (c *Convert) parseContent(content string) (string, []FailedMedia, error) {
	var failed []FailedMedia
	parsed, err := importer.ParseHTML(content, func(file *multipart.FileHeader, url string, err error) string {
		if err != nil {
			failed = append(failed, FailedMedia{URL: url, Error: err})
			return ""
		}

		//media, err := c.store.Media.Upload(file, c.owner.Token)
		//if err != nil {
		//	failed = append(failed, FailedMedia{URI: url, Error: err})
		//	return ""
		//}
		//
		//return media.URI
		return ""
	})

	if err != nil {
		return "", failed, err
	}

	return parsed, failed, nil
}

// getCategory
//
// Converts a 'Wordpress' category into a domain.Category
//
// Returns found category if it already exists.
// Returns newly created category if it doesnt exist.
// Returns errors.NOTFOUND if not category is attached to the post.
func (c *Convert) getCategory(categories []Category) (domain.Category, error) {
	const op = "WordpressConvertor.getCategory"

	if len(categories) == 0 {
		return domain.Category{}, &errors.Error{Code: errors.NOTFOUND, Message: "No category is attached to the post type.", Operation: op, Err: fmt.Errorf("no category found")}
	}

	wp := categories[0]

	return c.store.Categories.Create(domain.Category{
		Slug:     wp.URLSlug,
		Name:     wp.DisplayName,
		Resource: resource,
	})
}

// getSeoMeta
//
// Constructs domain.PostOptions and attaches meta titles and
// meta descriptions if the 'Yoast' plugin exists in
// 'Wordpress'.
func (c *Convert) getSeoMeta(title string, meta []Meta) domain.PostOptions {
	m := domain.PostOptions{
		Meta: &domain.PostMeta{
			Title: title,
			Twitter: domain.PostTwitter{
				Title: title,
			},
			Facebook: domain.PostFacebook{
				Title: title,
			},
		},
	}

	for _, v := range meta {
		if v.MetaKey == "_yoast_wpseo_metadesc" {
			m.Meta.Description = v.MetaValue
			m.Meta.Twitter.Description = v.MetaValue
			m.Meta.Facebook.Description = v.MetaValue
		}
	}

	return m
}

// findAuthor
//
// Looks through the array of authors attached to the Convert
// struct and returns the Author ID.
//
// Returns owner ID if there was an error obtaining the Wordpress
// authors or no author exists in the Convert authors array.
func (c *Convert) findAuthor(item Item) int {
	author, err := c.XML.AuthorForLogin(item.Creator)
	if err != nil {
		return c.owner.Id
	}

	for _, v := range c.authors {
		if v.Email == author.AuthorEmail {
			return v.Id
		}
	}

	return c.owner.Id
}

// populateAuthors
//
// Loops over the Wordpress authors and checks to see if they exist.
// If they dont, a new user will be created and an email will be
// sent with there their password. If they do exist, the author
// will be appended to the Convert author array.
// The user will be added to the FailedAuthors array in any case of error.
func (c *Convert) populateAuthors() domain.UsersParts {
	var users domain.UsersParts

	for _, v := range c.XML.Channel.Authors {
		exists := c.store.User.ExistsByEmail(v.AuthorEmail)
		if !exists {
			user, password, err := c.createUser(v)
			if err != nil {
				continue
			}

			color.Green.Println(fmt.Sprintf("User: %s Password: %s", user.Email, password))

			// if c.sendEmail {
			// User can't login!
			// FIX HERE
			//err = importer.SendNewPassword(user.HideCredentials(), password, c.store.Site.GetGlobalConfig())
			//if err != nil {
			//	color.Red.Println(err)
			//	continue
			//}
			//}

			users = append(users, user.HideCredentials())
			continue
		}

		user, err := c.store.User.FindByEmail(v.AuthorEmail)
		if err != nil {
			c.failAuthor(v.AuthorFirstName, v.AuthorLastName, v.AuthorEmail, err)
			continue
		}

		c.authors = append(c.authors, user)
	}

	return users
}

// createUser
//
// Generates a new password and continues to create a new User
// from the repository. If the user failed to be created it
// will be added to the FailedAuthors array.
//
// Returns the newly created password if successful.
// Returns an error if the user could not be created.
func (c *Convert) createUser(a Author) (domain.User, string, error) {
	password := encryption.CreatePassword()

	user := domain.UserCreate{
		User: domain.User{
			UserPart: domain.UserPart{
				FirstName: a.AuthorFirstName,
				LastName:  a.AuthorLastName,
				Email:     a.AuthorEmail,
				Role: domain.Role{
					Id: userRoleID,
				},
			},
		},
		Password:        password,
		ConfirmPassword: password,
	}

	u, err := c.store.User.Create(user)
	if err != nil {
		c.failAuthor(a.AuthorFirstName, a.AuthorLastName, a.AuthorEmail, err)
		return domain.User{}, "", err
	}

	c.authors = append(c.authors, u)

	return user.User, password, nil
}

// getStatus
//
// Converts the Wordpress status to Verbis specific status's.
func getStatus(status string) string {
	if status == "publish" {
		return "published"
	}
	return status
}

// failPost
//
// Append to the failed posts array.
func (c *Convert) failPost(item Item, media []FailedMedia, err error) {
	c.failed.Posts = append(c.failed.Posts, FailedPost{
		Post:  item,
		Media: media,
		Error: err,
	})
}

// Append
//
// Append to the failed authors array.
func (c *Convert) failAuthor(fName, lName, email string, err error) {
	c.failed.Authors = append(c.failed.Authors, FailedAuthor{
		FirstName: fName,
		LastName:  lName,
		Email:     email,
		Error:     err,
	})
}
