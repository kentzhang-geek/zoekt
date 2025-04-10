// Copyright 2016 Google Inc. All rights reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//    http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package web

import (
	"html/template"
	"log"
)

// Top provides the standard templates in parsed form
var Top = template.New("top").Funcs(Funcmap)

// TemplateText contains the text of the standard templates.
var TemplateText = map[string]string{
	"head": `
<head>
<meta charset="utf-8">
<meta http-equiv="X-UA-Compatible" content="IE=edge">
<meta name="viewport" content="width=device-width, initial-scale=1">
<!-- Licensed under MIT (https://github.com/twbs/bootstrap/blob/master/LICENSE) -->
<link rel="stylesheet" href="https://maxcdn.bootstrapcdn.com/bootstrap/3.3.7/css/bootstrap.min.css" integrity="sha384-BVYiiSIFeK1dGmJRAkycuHAHRg32OmUcww7on3RYdg4Va+PmSTsz/K68vbdEjh4u" crossorigin="anonymous">
<style>
  #navsearchbox { width: 350px !important; }
  #maxhits { width: 100px !important; }
  .label-dup {
    border-width: 1px !important;
    border-style: solid !important;
    border-color: #aaa !important;
    color: black;
  }
  .noselect {
    user-select: none;
  }
  a.label-dup:hover {
    color: black;
    background: #ddd;
  }
  .result {
    display: block;
    content: " ";
    visibility: hidden;
  }
  .container-results {
     overflow: auto;
     max-height: calc(100% - 72px);
  }
  .inline-pre {
     border: unset;
     background-color: unset;
     margin: unset;
     padding: unset;
     overflow: unset;
  }
  .copy-btn {
     margin-left: 2px;
     cursor: pointer;
     opacity: 0.6;
     font-size: 0.8em;
  }
  .copy-btn:hover {
     opacity: 1;
  }
  #copy-message {
     display: none;
     position: fixed;
     top: 20px;
     left: 50%;
     transform: translateX(-50%);
     padding: 8px 16px;
     background-color: rgba(0, 0, 0, 0.7);
     color: white;
     border-radius: 4px;
     z-index: 9999;
  }
  :target { background-color: #ccf; }
  table tbody tr td { border: none !important; padding: 2px !important; }
  
  /* Search history styles */
  .search-history-list {
     max-height: 60vh;
     overflow-y: auto;
     min-width: 300px;
     max-width: 400px;
  }
  .search-history-list li {
     width: 100%;
  }
  .search-history-list li a {
     text-overflow: ellipsis;
     overflow-x: auto;
     max-width: 100%;
     white-space: nowrap;
     display: block;
     padding-right: 15px;
  }
  .search-history-item {
     padding: 3px 20px;
     cursor: pointer;
  }
  .search-history-item:hover {
     background-color: #f5f5f5;
  }
  
  /* Favorite searches styles */
  .favorite-list {
     max-height: 60vh;
     overflow-y: auto;
     min-width: 300px;
     max-width: 400px;
  }
  .favorite-list li {
     width: 100%;
  }
  .favorite-list li a {
     text-overflow: ellipsis;
     overflow-x: auto;
     max-width: 100%;
     white-space: nowrap;
     display: block;
     padding-right: 15px;
  }
  .favorite-item {
     padding: 3px 20px;
     cursor: pointer;
  }
  .favorite-item:hover {
     background-color: #f5f5f5;
  }
  .favorite-btn {
     color: #ffc107;
     cursor: pointer;
     margin-left: 8px;
  }
  .favorite-btn:hover {
     color: #ffdb58;
  }
</style>
</head>
  `,

	"jsdep": `
<script src="https://ajax.googleapis.com/ajax/libs/jquery/1.12.4/jquery.min.js"></script>
<script src="https://maxcdn.bootstrapcdn.com/bootstrap/3.3.7/js/bootstrap.min.js" integrity="sha384-Tc5IQib027qvyjSMfHjOMaLkfuWVxZxUPnCJA7l2mCWNIpG9mGCD8wGNIcPD7Txa" crossorigin="anonymous"></script>

<script>
// Search history functionality
(function() {
  const SEARCH_HISTORY_KEY = 'zoekt_search_history';
  const FAVORITE_SEARCHES_KEY = 'zoekt_favorite_searches';
  const MAX_HISTORY_ITEMS = 20;
  const MAX_FAVORITE_ITEMS = 50;
  
  // Save a search query to history
  function saveToSearchHistory(query) {
    if (!query || query.trim() === '') return;
    
    try {
      // Get existing history or create new array
      let history = JSON.parse(localStorage.getItem(SEARCH_HISTORY_KEY) || '[]');
      
      // Remove existing entry if already exists (to move it to the top)
      history = history.filter(item => item !== query);
      
      // Add new query to the beginning
      history.unshift(query);
      
      // Limit history size
      if (history.length > MAX_HISTORY_ITEMS) {
        history = history.slice(0, MAX_HISTORY_ITEMS);
      }
      
      // Save back to localStorage
      localStorage.setItem(SEARCH_HISTORY_KEY, JSON.stringify(history));
    } catch (error) {
      console.error('Failed to save search history:', error);
    }
  }
  
  // Load and display search history
  function loadSearchHistory() {
    try {
      const history = JSON.parse(localStorage.getItem(SEARCH_HISTORY_KEY) || '[]');
      
      // Find ALL history lists on the page (there may be multiple)
      const historyLists = document.querySelectorAll('.search-history-list');
      
      historyLists.forEach(historyList => {
        // Clear existing history items but keep the "Clear History" option
        const divider = historyList.querySelector('.divider');
        if (!divider) return; // Skip if list doesn't have expected structure
        
        // Remove all items before the divider
        while (historyList.firstChild !== divider) {
          historyList.removeChild(historyList.firstChild);
        }
        
        // Add history items
        if (history.length === 0) {
          const emptyItem = document.createElement('li');
          emptyItem.className = 'search-history-item';
          emptyItem.textContent = 'No search history';
          emptyItem.style.fontStyle = 'italic';
          emptyItem.style.color = '#999';
          historyList.insertBefore(emptyItem, divider);
        } else {
          history.forEach(query => {
            const item = document.createElement('li');
            const link = document.createElement('a');
            link.href = 'search?q=' + encodeURIComponent(query);
            link.textContent = query;
            link.title = query;
            item.appendChild(link);
            historyList.insertBefore(item, divider);
          });
        }
      });
    } catch (error) {
      console.error('Failed to load search history:', error);
    }
  }
  
  // Clear search history
  window.clearSearchHistory = function() {
    try {
      localStorage.setItem(SEARCH_HISTORY_KEY, '[]');
      loadSearchHistory();
    } catch (error) {
      console.error('Failed to clear search history:', error);
    }
  }
  
  // Save a search query to favorites
  window.addToFavorites = function(query) {
    if (!query || query.trim() === '') return;
    
    try {
      // Get existing favorites or create new array
      let favorites = JSON.parse(localStorage.getItem(FAVORITE_SEARCHES_KEY) || '[]');
      
      // Don't add if it already exists
      if (favorites.includes(query)) {
        showMessage("Already in favorites");
        return;
      }
      
      // Add new query to the favorites
      favorites.push(query);
      
      // Sort alphabetically
      favorites.sort();
      
      // Limit size
      if (favorites.length > MAX_FAVORITE_ITEMS) {
        favorites = favorites.slice(0, MAX_FAVORITE_ITEMS);
      }
      
      // Save back to localStorage
      localStorage.setItem(FAVORITE_SEARCHES_KEY, JSON.stringify(favorites));
      
      // Reload favorites in UI
      loadFavorites();
      
      // Show confirmation
      showMessage("Added to favorites");
    } catch (error) {
      console.error('Failed to save favorite:', error);
    }
  }
  
  // Remove a search query from favorites
  window.removeFromFavorites = function(query) {
    try {
      // Get existing favorites
      let favorites = JSON.parse(localStorage.getItem(FAVORITE_SEARCHES_KEY) || '[]');
      
      // Remove the query
      favorites = favorites.filter(item => item !== query);
      
      // Save back to localStorage
      localStorage.setItem(FAVORITE_SEARCHES_KEY, JSON.stringify(favorites));
      
      // Reload favorites in UI
      loadFavorites();
      
      // Show confirmation
      showMessage("Removed from favorites");
    } catch (error) {
      console.error('Failed to remove favorite:', error);
    }
  }
  
  // Clear all favorites
  window.clearFavorites = function() {
    try {
      localStorage.setItem(FAVORITE_SEARCHES_KEY, '[]');
      loadFavorites();
      showMessage("Favorites cleared");
    } catch (error) {
      console.error('Failed to clear favorites:', error);
    }
  }
  
  // Load and display favorites
  function loadFavorites() {
    try {
      const favorites = JSON.parse(localStorage.getItem(FAVORITE_SEARCHES_KEY) || '[]');
      
      // Find ALL favorites lists on the page (there may be multiple)
      const favoritesLists = document.querySelectorAll('.favorite-list');
      
      favoritesLists.forEach(favoritesList => {
        // Clear existing favorites items but keep the management options
        const divider = favoritesList.querySelector('.divider');
        if (!divider) return; // Skip if list doesn't have expected structure
        
        // Remove all items before the divider
        while (favoritesList.firstChild !== divider) {
          favoritesList.removeChild(favoritesList.firstChild);
        }
        
        // Add favorites
        if (favorites.length === 0) {
          const emptyItem = document.createElement('li');
          emptyItem.className = 'favorite-item';
          emptyItem.textContent = 'No favorite searches';
          emptyItem.style.fontStyle = 'italic';
          emptyItem.style.color = '#999';
          favoritesList.insertBefore(emptyItem, divider);
        } else {
          favorites.forEach(query => {
            const item = document.createElement('li');
            item.className = 'favorite-item';
            
            const link = document.createElement('a');
            link.href = 'search?q=' + encodeURIComponent(query);
            link.textContent = query;
            link.title = query;
            
            const removeBtn = document.createElement('a');
            removeBtn.className = 'pull-right text-danger';
            removeBtn.style.marginLeft = '10px';
            removeBtn.innerHTML = '&times;';
            removeBtn.href = '#';
            removeBtn.title = 'Remove from favorites';
            removeBtn.onclick = function(e) {
              e.preventDefault();
              e.stopPropagation();
              removeFromFavorites(query);
              return false;
            };
            
            item.appendChild(link);
            item.appendChild(removeBtn);
            favoritesList.insertBefore(item, divider);
          });
        }
      });
    } catch (error) {
      console.error('Failed to load favorites:', error);
    }
  }
  
  // Check if a query is in favorites
  window.isInFavorites = function(query) {
    try {
      const favorites = JSON.parse(localStorage.getItem(FAVORITE_SEARCHES_KEY) || '[]');
      return favorites.includes(query);
    } catch (error) {
      console.error('Failed to check favorites:', error);
      return false;
    }
  }
  
  // Show a message in the UI
  function showMessage(message) {
    // Create message element if it doesn't exist
    let messageEl = document.getElementById("copy-message");
    if (!messageEl) {
      messageEl = document.createElement("div");
      messageEl.id = "copy-message";
      document.body.appendChild(messageEl);
    }
    
    // Set message and show
    messageEl.textContent = message;
    messageEl.style.display = "block";
    
    // Hide after delay
    setTimeout(() => {
      messageEl.style.display = "none";
    }, 1500);
  }
  
  // Make showMessage available globally
  window.showMessage = showMessage;
  
  // Initialize on page load
  document.addEventListener('DOMContentLoaded', function() {
    console.log('Document loaded, initializing search history and favorites');
    
    // Add global click handler to ensure dropdown buttons work
    $(document).ready(function() {
      $('.dropdown-toggle').dropdown();
      console.log('Bootstrap dropdowns initialized');
    });
    
    loadSearchHistory();
    loadFavorites();
    
    // Add current search to history if on search page
    const navSearchBox = document.getElementById('navsearchbox');
    const mainSearchBox = document.getElementById('searchbox');
    
    // Check navigation search box
    if (navSearchBox && navSearchBox.value.trim() !== '') {
      saveToSearchHistory(navSearchBox.value.trim());
    }
    
    // Check main search box
    if (mainSearchBox && mainSearchBox.value.trim() !== '') {
      saveToSearchHistory(mainSearchBox.value.trim());
    }
    
    // Set up form submission behavior to save search history
    const searchForms = document.querySelectorAll('form[action="search"]');
    searchForms.forEach(form => {
      form.addEventListener('submit', function() {
        const query = this.querySelector('input[name="q"]').value;
        if (query && query.trim() !== '') {
          saveToSearchHistory(query.trim());
        }
      });
    });
  });
})();
</script>
`,

	"searchbox": `
<form action="search">
  <div class="form-group form-group-lg">
    <div class="input-group input-group-lg">
      <input class="form-control" placeholder="Search for some code..." autofocus
              {{if .Query}}
              value={{.Query}}
              {{end}}
              id="searchbox" type="text" name="q">
      <div class="input-group-btn">
        <!-- History dropdown -->
        <div class="btn-group">
          <button type="button" class="btn btn-default dropdown-toggle" data-toggle="dropdown" aria-haspopup="true" aria-expanded="false" title="Search History">
            <span class="glyphicon glyphicon-time" aria-hidden="true"></span>
            <span class="caret"></span>
          </button>
          <ul class="dropdown-menu dropdown-menu-right search-history-list">
            <!-- Search history will be populated here by JavaScript -->
            <li class="divider" role="separator"></li>
            <li><a href="#" onclick="clearSearchHistory(); return false;">Clear History</a></li>
          </ul>
        </div>
        
        <!-- Favorites dropdown -->
        <div class="btn-group">
          <button type="button" class="btn btn-default dropdown-toggle" data-toggle="dropdown" aria-haspopup="true" aria-expanded="false" title="Favorite Searches">
            <span class="glyphicon glyphicon-star" aria-hidden="true"></span>
            <span class="caret"></span>
          </button>
          <ul class="dropdown-menu dropdown-menu-right favorite-list">
            <!-- Favorite searches will be populated here by JavaScript -->
            <li class="divider" role="separator"></li>
            <li><a href="#" onclick="clearFavorites(); return false;">Clear Favorites</a></li>
          </ul>
        </div>
        
        <button class="btn btn-primary">Search</button>
      </div>
    </div>
  </div>
</form>
`,

	"navbar": `
<nav class="navbar navbar-default">
  <div class="container-fluid">
    <div class="navbar-header">
      <a class="navbar-brand" href="/">Zoekt</a>
      <button type="button" class="navbar-toggle collapsed" data-toggle="collapse" data-target="#navbar-collapse" aria-expanded="false">
        <span class="sr-only">Toggle navigation</span>
        <span class="icon-bar"></span>
        <span class="icon-bar"></span>
        <span class="icon-bar"></span>
      </button>
    </div>
    <div class="navbar-collapse collapse" id="navbar-collapse" aria-expanded="false" style="height: 1px;">
      <form class="navbar-form navbar-left" action="search">
        <div class="form-group">
          <div class="input-group">
            <input class="form-control"
                  placeholder="Search for some code..." role="search"
                  id="navsearchbox" type="text" name="q" autocomplete="off" autofocus
                  {{if .Query}}
                  value={{.Query}}
                  {{end}}>
            <div class="input-group-btn">
              <!-- History dropdown -->
              <div class="btn-group">
                <button type="button" class="btn btn-default dropdown-toggle" data-toggle="dropdown" aria-haspopup="true" aria-expanded="false" title="Search History">
                  <span class="glyphicon glyphicon-time" aria-hidden="true"></span>
                  <span class="caret"></span>
                </button>
                <ul class="dropdown-menu dropdown-menu-right search-history-list">
                  <!-- Search history will be populated here by JavaScript -->
                  <li class="divider" role="separator"></li>
                  <li><a href="#" onclick="clearSearchHistory(); return false;">Clear History</a></li>
                </ul>
              </div>
              
              <!-- Favorites dropdown -->
              <div class="btn-group">
                <button type="button" class="btn btn-default dropdown-toggle" data-toggle="dropdown" aria-haspopup="true" aria-expanded="false" title="Favorite Searches">
                  <span class="glyphicon glyphicon-star" aria-hidden="true"></span>
                  <span class="caret"></span>
                </button>
                <ul class="dropdown-menu dropdown-menu-right favorite-list">
                  <!-- Favorite searches will be populated here by JavaScript -->
                  <li class="divider" role="separator"></li>
                  <li><a href="#" onclick="clearFavorites(); return false;">Clear Favorites</a></li>
                </ul>
              </div>
            </div>
          </div>
          <div class="input-group">
            <div class="input-group-addon">Max Results</div>
            <input class="form-control" type="number" id="maxhits" name="num" value="{{.Num}}">
          </div>
          <button class="btn btn-primary">Search</button>
          <!--Hack: we use a hidden form field to keep track of the debug flag across searches-->
          {{if .Debug}}<input id="debug" name="debug" type="hidden" value="{{.Debug}}">{{end}}
        </div>
      </form>
    </div>
  </div>
</nav>
<script>
document.onkeydown=function(e){
  var e = e || window.event;
  if (e.key == "/") {
    var navbox = document.getElementById("navsearchbox");
    if (document.activeElement !== navbox) {
      navbox.focus();
      return false;
    }
  }
};
</script>
`,
	"search": `
<html>
{{template "head"}}
<title>Zoekt, en gij zult spinazie eten</title>
<body>
  <div class="jumbotron">
    <div class="container">
      {{template "searchbox" .Last}}
    </div>
  </div>

  <div class="container">
    <div class="row">
      <div class="col-md-8">
        <h3>Search examples:</h3>
        <dl class="dl-horizontal">
          <dt><a href="search?q=needle">needle</a></dt><dd>search for "needle"</dd>
          <dt><a href="search?q=thread+or+needle">thread or needle</a></dt><dd>search for either "thread" or "needle"</dd>
          <dt><a href="search?q=class+needle">class needle</a></span></dt><dd>search for files containing both "class" and "needle"</dd>
          <dt><a href="search?q=class+Needle">class Needle</a></dt><dd>search for files containing both "class" (case insensitive) and "Needle" (case sensitive)</dd>
          <dt><a href="search?q=class+Needle+case:yes">class Needle case:yes</a></dt><dd>search for files containing "class" and "Needle", both case sensitively</dd>
          <dt><a href="search?q=%22class Needle%22">"class Needle"</a></dt><dd>search for files with the phrase "class Needle"</dd>
          <dt><a href="search?q=needle+-hay">needle -hay</a></dt><dd>search for files with the word "needle" but not the word "hay"</dd>
          <dt><a href="search?q=path+file:java">path file:java</a></dt><dd>search for the word "path" in files whose name contains "java"</dd>
          <dt><a href="search?q=needle+lang%3Apython&num=50">needle lang:python</a></dt><dd>search for "needle" in Python source code</dd>
          <dt><a href="search?q=f:%5C.c%24">f:\.c$</a></dt><dd>search for files whose name ends with ".c"</dd>
          <dt><a href="search?q=path+-file:java">path -file:java</a></dt><dd>search for the word "path" excluding files whose name contains "java"</dd>
          <dt><a href="search?q=foo.*bar">foo.*bar</a></dt><dd>search for the regular expression "foo.*bar"</dd>
          <dt><a href="search?q=-%28Path File%29 Stream">-(Path File) Stream</a></dt><dd>search "Stream", but exclude files containing both "Path" and "File"</dd>
          <dt><a href="search?q=-Path%5c+file+Stream">-Path\ file Stream</a></dt><dd>search "Stream", but exclude files containing "Path File"</dd>
          <dt><a href="search?q=sym:data">sym:data</a></span></dt><dd>search for symbol definitions containing "data"</dd>
          <dt><a href="search?q=phone+r:droid">phone r:droid</a></dt><dd>search for "phone" in repositories whose name contains "droid"</dd>
          <dt><a href="search?q=phone+archived:no">phone archived:no</a></dt><dd>search for "phone" in repositories that are not archived</dd>
          <dt><a href="search?q=phone+fork:no">phone fork:no</a></dt><dd>search for "phone" in repositories that are not forks</dd>
          <dt><a href="search?q=phone+public:no">phone public:no</a></dt><dd>search for "phone" in repositories that are not public</dd>
          <dt><a href="search?q=phone+b:master">phone b:master</a></dt><dd>for Git repos, find "phone" in files in branches whose name contains "master".</dd>
          <dt><a href="search?q=phone+b:HEAD">phone b:HEAD</a></dt><dd>for Git repos, find "phone" in the default ('HEAD') branch.</dd>
        </dl>
      </div>
      <div class="col-md-4">
        <h3>To list repositories, try:</h3>
        <dl class="dl-horizontal">
          <dt><a href="search?q=r:droid">r:droid</a></dt><dd>list repositories whose name contains "droid".</dd>
          <dt><a href="search?q=r:go+-r:google">r:go -r:google</a></dt><dd>list repositories whose name contains "go" but not "google".</dd>
        </dl>
      </div>
    </div>
  </div>
  <nav class="navbar navbar-default navbar-bottom">
    <div class="container">
      {{template "footerBoilerplate"}}
      <p class="navbar-text navbar-right">
        Used {{HumanUnit .Stats.IndexBytes}} mem for
        {{.Stats.Documents}} documents ({{HumanUnit .Stats.ContentBytes}})
        from {{.Stats.Repos}} repositories.
      </p>
    </div>
  </nav>
  {{ template "jsdep"}}
  <script>
    // Initialize dropdowns explicitly for this page
    $(document).ready(function() {
      $('.dropdown-toggle').dropdown();
    });
  </script>
</body>
</html>
`,
	"footerBoilerplate": `<a class="navbar-text" href="about">About</a>`,
	"results": `
<html>
{{template "head"}}
<title>Results for {{.QueryStr}}</title>
<script>
  function zoektAddQ(atom) {
      window.location.href = "/search?q=" + escape("{{.QueryStr}}" + " " + atom) +
	  "&" + "num=" + {{.Last.Num}};
  }
  
  function copyCodeLink(fileName, lineNum) {
      const text = "codelink://" + fileName + ":" + lineNum;
      navigator.clipboard.writeText(text).then(function() {
          showCopyMessage("Copied link to clipboard");
      }).catch(function(err) {
          console.error("Error copying to clipboard: ", err);
          showCopyMessage("Failed to copy link");
      });
  }
  
  function showCopyMessage(message) {
      // Create message element if it doesn't exist
      let messageEl = document.getElementById("copy-message");
      if (!messageEl) {
          messageEl = document.createElement("div");
          messageEl.id = "copy-message";
          document.body.appendChild(messageEl);
      }
      
      // Set message and show
      messageEl.textContent = message;
      messageEl.style.display = "block";
      
      // Hide after delay
      setTimeout(() => {
          messageEl.style.display = "none";
      }, 1500);
  }
</script>
<body id="results">
  {{template "navbar" .Last}}
  <div class="container-fluid container-results">
    <h5>
      {{if .Stats.Crashes}}<br><b>{{.Stats.Crashes}} shards crashed</b><br>{{end}}
      {{ $fileCount := len .FileMatches }}
      Found {{.Stats.MatchCount}} results in {{.Stats.FileCount}} files{{if or (lt $fileCount .Stats.FileCount) (or (gt .Stats.ShardsSkipped 0) (gt .Stats.FilesSkipped 0)) }},
        showing top {{ $fileCount }} files (<a rel="nofollow"
           href="search?q={{.Last.Query}}&num={{More .Last.Num}}">show more</a>).
      {{else}}.{{end}}
      {{if .QueryStr}}
      <span class="pull-right">
        <button id="favorite-toggle" class="btn btn-xs" onclick="toggleFavorite('{{.QueryStr}}'); return false;">
          <span class="glyphicon glyphicon-star"></span>
          <span id="favorite-text">Add to favorites</span>
        </button>
      </span>
      <script>
        // Initialize favorite button state
        document.addEventListener('DOMContentLoaded', function() {
          updateFavoriteButton('{{.QueryStr}}');
        });
        
        function toggleFavorite(query) {
          if (isInFavorites(query)) {
            removeFromFavorites(query);
          } else {
            addToFavorites(query);
          }
          updateFavoriteButton(query);
        }
        
        function updateFavoriteButton(query) {
          const btn = document.getElementById('favorite-toggle');
          const text = document.getElementById('favorite-text');
          
          if (isInFavorites(query)) {
            btn.className = 'btn btn-xs btn-warning';
            text.textContent = 'Remove from favorites';
          } else {
            btn.className = 'btn btn-xs btn-default';
            text.textContent = 'Add to favorites';
          }
        }
      </script>
      {{end}}
    </h5>
    {{range .FileMatches}}
    <table class="table table-hover table-condensed">
      <thead>
        <tr>
          <th>
            {{if .URL}}<a name="{{.ResultID}}" class="result"></a><a href="{{.URL}}" >{{else}}<a name="{{.ResultID}}">{{end}}
            <small>
              {{.Repo}}:{{.FileName}} {{if .ScoreDebug}}<i>({{.ScoreDebug}})</i>{{end}}</a>:
              <span style="font-weight: normal">[ {{if .Branches}}{{range .Branches}}<span class="label label-default">{{.}}</span>,{{end}}{{end}} ]</span>
              {{if .Language}}<button
                   title="restrict search to files written in {{.Language}}"
                   onclick="zoektAddQ('lang:&quot;{{.Language}}&quot;')" class="label label-primary">language {{.Language}}</button></span>{{end}}
              {{if .DuplicateID}}<a class="label label-dup" href="#{{.DuplicateID}}">Duplicate result</a>{{end}}
            </small>
          </th>
        </tr>
      </thead>
      {{if not .DuplicateID}}
      <tbody>
        {{range .Matches}}
        {{if gt .LineNum 0}}
        <tr>
          <td style="background-color: rgba(238, 238, 255, 0.6);">
            <pre class="inline-pre"><span class="noselect">{{if .URL}}<a href="{{.URL}}">{{end}}<u>{{.LineNum}}</u>{{if .URL}}</a>{{end}}<a href="#" class="copy-btn" title="Copy code link" onclick="copyCodeLink('{{.FileName}}', {{.LineNum}}); return false;">📋</a>: </span>{{range .Fragments}}{{LimitPre 100 .Pre}}<b>{{.Match}}</b>{{LimitPost 100 (TrimTrailingNewline .Post)}}{{end}} {{if .ScoreDebug}}<i>({{.ScoreDebug}})</i>{{end}}</pre>
          </td>
        </tr>
        {{end}}
      </tbody>
      {{end}}
      {{end}}
    </table>
    {{end}}

  <nav class="navbar navbar-default navbar-bottom">
    <div class="container">
      {{template "footerBoilerplate"}}
      <p class="navbar-text navbar-right">
      Took {{.Stats.Duration}}{{if .Stats.Wait}} (queued: {{.Stats.Wait}}){{end}} for
      {{HumanUnit .Stats.IndexBytesLoaded}}B index data,
      {{.Stats.NgramMatches}} ngram matches,
      {{.Stats.FilesConsidered}} docs considered,
      {{.Stats.FilesLoaded}} docs ({{HumanUnit .Stats.ContentBytesLoaded}}B) loaded,
      {{.Stats.ShardsScanned}} shards scanned,
      {{.Stats.ShardsSkippedFilter}} shards filtered
      {{- if or .Stats.FilesSkipped .Stats.ShardsSkipped -}}
        , {{.Stats.FilesSkipped}} docs skipped, {{.Stats.ShardsSkipped}} shards skipped
      {{- end -}}
	  .
      </p>
    </div>
  </nav>
  </div>
  {{ template "jsdep"}}
</body>
</html>
`,

	"repolist": `
<html>
{{template "head"}}
<body id="results">
  <div class="container">
    {{template "navbar" .Last}}
    <div><b>
    Found {{.Stats.Repos}} repositories ({{.Stats.Documents}} files, {{HumanUnit .Stats.ContentBytes}}B content)
    </b></div>
    <table class="table table-hover table-condensed">
      <thead>
	<tr>
	  {{- define "q"}}q={{.Last.Query}}{{if (gt .Last.Num 0)}}&num={{.Last.Num}}{{end}}{{end}}
	  <th>Name <a href="/search?{{template "q" .}}&order=name">▼</a><a href="/search?{{template "q" .}}&order=revname">▲</a></th>
	  <th>Last updated <a href="/search?{{template "q" .}}&order=revtime">▼</a><a href="/search?{{template "q" .}}&order=time">▲</a></th>
	  <th>Branches</th>
	  <th>Size <a href="/search?{{template "q" .}}&order=revsize">▼</a><a href="/search?{{template "q" .}}&order=size">▲</a></th>
	  <th>RAM <a href="/search?{{template "q" .}}&order=revram">▼</a><a href="/search?{{template "q" .}}&order=ram">▲</a></th>
	</tr>
      </thead>
      <tbody>
	{{range .Repos -}}
	<tr>
	  <td>{{if .URL}}<a href="{{.URL}}">{{end}}{{.Name}}{{if .URL}}</a>{{end}}</td>
	  <td><small>{{.IndexTime.Format "Jan 02, 2006 15:04"}}</small></td>
	  <td style="vertical-align: middle;">
	    {{- range .Branches -}}
	    {{if .URL}}<tt><a class="label label-default small" href="{{.URL}}">{{end}}{{.Name}}{{if .URL}}</a> </tt>{{end}}&nbsp;
	    {{- end -}}
	  </td>
	  <td><small>{{HumanUnit .Files}} files ({{HumanUnit .Size}}B)</small></td>
	  <td><small>{{HumanUnit .MemorySize}}B</td>
	</tr>
	{{end}}
      </tbody>
    </table>
  </div>

  <nav class="navbar navbar-default navbar-bottom">
    <div class="container">
      {{template "footerBoilerplate"}}
      <p class="navbar-text navbar-right">
      </p>
    </div>
  </nav>

  {{ template "jsdep"}}
</body>
</html>
`,

	"print": `
<html>
  {{template "head"}}
  <title>{{.Repo}}:{{.Name}}</title>
<body id="results">
    <script>
        function copyToClipboard(text) {
            navigator.clipboard.writeText(text).then(function() {
                showCopyMessage("Copied link to clipboard");
            }).catch(function(err) {
                console.error("Error copying to clipboard: ", err);
                showCopyMessage("Failed to copy link");
            });
        }
        
        function showCopyMessage(message) {
            // Create message element if it doesn't exist
            let messageEl = document.getElementById("copy-message");
            if (!messageEl) {
                messageEl = document.createElement("div");
                messageEl.id = "copy-message";
                document.body.appendChild(messageEl);
            }
            
            // Set message and show
            messageEl.textContent = message;
            messageEl.style.display = "block";
            
            // Hide after delay
            setTimeout(() => {
                messageEl.style.display = "none";
            }, 1500);
        }
    </script>
  {{template "navbar" .Last}}
  <div class="container-fluid container-results" >
     <div><b>{{.Name}}</b></div>
     <div class="table table-hover table-condensed" style="overflow:auto; background: #eef;">
{{ $fname := .Name }}
       {{ range $index, $ln := .Lines }}
	 <pre id="l{{Inc $index}}" class="inline-pre"><span class="noselect"><a href="#l{{Inc $index}}">{{Inc $index}}</a><a href="#" class="copy-btn" title="Copy code link" onclick="copyToClipboard('codelink://{{$fname}}:{{Inc $index}}'); return false;">📋</a>: </span>{{$ln}}</pre>       {{ end }}
     </div>
  <nav class="navbar navbar-default navbar-bottom">
    <div class="container">
      {{template "footerBoilerplate"}}
      <p class="navbar-text navbar-right">
      </p>
    </div>
  </nav>
  </div>
 {{ template "jsdep"}}
</body>
</html>
`,

	"about": `

<html>
  {{template "head"}}
  <title>About <em>zoekt</em></title>
<body>


  <div class="jumbotron">
    <div class="container">
      {{template "searchbox" .Last}}
    </div>
  </div>

  <div class="container">
    <p>
      This is <a href="http://github.com/sourcegraph/zoekt"><em>zoekt</em> (IPA: /zukt/)</a>,
      an open-source full text search engine. It's pronounced roughly as you would
      pronounce "zooked" in English.
    </p>
    <p>
    {{if .Version}}<em>Zoekt</em> version {{.Version}}, uptime{{else}}Uptime{{end}} {{.Uptime}}
    </p>

    <p>
    Used {{HumanUnit .Stats.IndexBytes}} memory for
    {{.Stats.Documents}} documents ({{HumanUnit .Stats.ContentBytes}})
    from {{.Stats.Repos}} repositories.
    </p>
  </div>

  <nav class="navbar navbar-default navbar-bottom">
    <div class="container">
      {{template "footerBoilerplate"}}
      <p class="navbar-text navbar-right">
      </p>
    </div>
  </nav>
`,
	"robots": `
user-agent: *
disallow: /search
`,
}

func init() {
	for k, v := range TemplateText {
		_, err := Top.New(k).Parse(v)
		if err != nil {
			log.Panicf("parse(%s): %v:", k, err)
		}
	}
}
